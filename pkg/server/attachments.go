package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"codeforge/pkg/llm"
)

const (
	maxUploadFile  = 20 << 20
	maxUploadBody  = 21 << 20
	maxImageBytes  = 5 << 20
	maxImageTotal  = 20 << 20
	maxImages      = 4
	maxImagePixels = 16_000_000
)

func openAttachmentWorkspace(workspace string) (*os.Root, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("还没有选择有效工作区")
	}
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(real)
}

func attachmentRelative(root *os.Root, workspace, name string) (string, error) {
	if name == "" || strings.Contains(name, "://") || strings.HasPrefix(name, "//") || strings.HasPrefix(name, `\\`) {
		return "", fmt.Errorf("附件必须是工作区内的本地文件")
	}
	name = filepath.FromSlash(strings.ReplaceAll(name, `\`, "/"))
	if filepath.IsAbs(name) {
		var err error
		name, err = filepath.Rel(workspace, name)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsLocal(name) || strings.Contains(name, ":") {
		return "", fmt.Errorf("附件路径越出工作区范围")
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root.Name(), name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root.Name(), real)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("附件真实路径越出工作区范围")
	}
	return rel, nil
}

func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace := s.fs.Root()
	root, err := openAttachmentWorkspace(workspace)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "上传文件", err)
		return
	}
	defer root.Close()
	fail := func(err error) {
		status := http.StatusBadRequest
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, "上传文件", err)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	defer r.Body.Close()
	if r.ContentLength > maxUploadBody {
		fail(&http.MaxBytesError{Limit: maxUploadBody})
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		fail(err)
		return
	}
	var dir *os.Root
	var file *os.File
	var staged, original string
	complete := false
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if dir != nil {
			if !complete && staged != "" {
				_ = dir.Remove(staged)
			}
			_ = dir.Close()
		}
	}()
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(err)
			return
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil || part.FormName() != "file" || params["filename"] == "" || file != nil {
			fail(fmt.Errorf("请求必须包含唯一的 file 文件字段"))
			return
		}
		original = params["filename"]
		if original == "." || original == ".." || strings.ContainsAny(original, "/\\:\x00\r\n") {
			fail(fmt.Errorf("文件名不能包含路径"))
			return
		}
		if err := root.Mkdir("attachments", 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			fail(err)
			return
		}
		rel, err := attachmentRelative(root, workspace, "attachments")
		if err != nil {
			fail(err)
			return
		}
		dir, err = root.OpenRoot(rel)
		if err != nil {
			fail(err)
			return
		}
		ext := filepath.Ext(original)
		if len(ext) > 16 || strings.IndexFunc(ext, func(r rune) bool {
			return r != '.' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
		}) >= 0 {
			ext = ""
		}
		for {
			var id [16]byte
			if _, err = rand.Read(id[:]); err != nil {
				fail(err)
				return
			}
			name := hex.EncodeToString(id[:]) + ext
			file, err = dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				fail(err)
				return
			}
			staged = name
			break
		}
		n, err := io.Copy(file, io.LimitReader(part, maxUploadFile+1))
		if err != nil {
			fail(err)
			return
		}
		if n > maxUploadFile {
			fail(&http.MaxBytesError{Limit: maxUploadFile})
			return
		}
		if err := file.Close(); err != nil {
			fail(err)
			return
		}
	}
	if file == nil {
		fail(fmt.Errorf("缺少 file 文件字段"))
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		fail(err)
		return
	}
	complete = true
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "staged_path": "attachments/" + staged, "name": original})
}

func readAttachmentImages(workspace string, attachments []string) ([]llm.ContentBlock, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	root, err := openAttachmentWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var images []llm.ContentBlock
	total := 0
	for _, name := range attachments {
		block, err := readAttachmentImage(root, workspace, name)
		if err != nil {
			return nil, fmt.Errorf("附件 %q: %w", name, err)
		}
		if block == nil {
			continue
		}
		padding := len(block.Data) - len(strings.TrimRight(block.Data, "="))
		total += base64.StdEncoding.DecodedLen(len(block.Data)) - padding
		if len(images) >= maxImages || total > maxImageTotal {
			return nil, fmt.Errorf("图片最多4张，总大小不能超过20MiB")
		}
		images = append(images, *block)
	}
	return images, nil
}

func readAttachmentImage(root *os.Root, workspace, name string) (*llm.ContentBlock, error) {
	rel, err := attachmentRelative(root, workspace, name)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("附件必须是普通文件")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("附件必须是普通文件")
	}
	header := make([]byte, 512)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	header = header[:n]
	mediaType := http.DetectContentType(header)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		switch strings.ToLower(filepath.Ext(name)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			return nil, fmt.Errorf("图片内容无效或已损坏")
		}
		return nil, nil
	}
	if info.Size() > maxImageBytes {
		return nil, fmt.Errorf("每张图片不能超过5MiB")
	}
	data, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(header), file), maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("每张图片不能超过5MiB")
	}
	if err := validateAttachmentImage(data, mediaType); err != nil {
		return nil, err
	}
	return &llm.ContentBlock{Type: llm.BlockImage, MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}, nil
}

func validateAttachmentImage(data []byte, mediaType string) error {
	var width, height int
	if mediaType == "image/webp" {
		if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
			return fmt.Errorf("WebP签名或长度无效")
		}
		size := uint64(binary.LittleEndian.Uint32(data[16:20]))
		if size+20 > uint64(len(data)) {
			return fmt.Errorf("WebP数据已截断")
		}
		switch string(data[12:16]) {
		case "VP8 ":
			if size < 10 || !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
				return fmt.Errorf("WebP VP8签名无效")
			}
			width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
			height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		case "VP8L":
			if size < 5 || data[20] != 0x2f || data[24]>>5 != 0 {
				return fmt.Errorf("WebP VP8L签名无效")
			}
			bits := binary.LittleEndian.Uint32(data[21:25])
			width, height = int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1
		case "VP8X":
			if size != 10 || len(data) <= 30 {
				return fmt.Errorf("WebP VP8X签名无效")
			}
			width = 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16
			height = 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16
		default:
			return fmt.Errorf("WebP签名无效")
		}
	} else {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("图片配置无效: %w", err)
		}
		width, height = cfg.Width, cfg.Height
	}
	if width <= 0 || height <= 0 || int64(width)*int64(height) > maxImagePixels {
		return fmt.Errorf("图片像素不能超过1600万")
	}
	if mediaType != "image/webp" {
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return fmt.Errorf("图片已损坏: %w", err)
		}
	}
	return nil
}
