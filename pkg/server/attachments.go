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

	// maxVideoBytes / maxVideos 是视频的独立预算，**不与图片预算合并计算**。
	//
	// 上限刻意沿用 maxUploadFile（20MiB），不因为「视频通常更大」就放宽：
	// 视频是**整段 base64 塞进请求体**（再经 data URI 前缀放大约 1.37 倍），
	// 一段 20MiB 的视频会让单次请求体到 ~27MiB，且明文与 base64 两份
	// 同时驻留内存。再放宽就要改 multipart 的 body 级上限
	//（http.MaxBytesReader，那是一道兜底刹车，不该为附件类型单独抬高）。
	//
	// 20MiB 约等于 1080p 的 8~10 秒，够「贴一段录屏问一句」这个真实用法。
	// 真要看长视频，正确做法是先自行抽帧再附图 —— 那条路本地不需要解码器。
	maxVideoBytes = 20 << 20
	maxVideos     = 1
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

// readAttachmentMedia 把本轮附件读成可送给模型的内容块。
//
// 图片与视频都在这里产出，原因只有一条：**能力判定不在本层**。
// 本层不知道当前是哪个模型（那是 agent 的事，见 agent/capability.go 的能力门），
// 所以它只负责「如实识别」；模型收不了的那一类由上层丢弃并提示用户。
// 若在这里就按模型过滤，就得把模型信息透传进来，附件层与模型配置就耦合了。
func readAttachmentMedia(workspace string, attachments []string) ([]llm.ContentBlock, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	root, err := openAttachmentWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var blocks []llm.ContentBlock
	imageTotal, videos := 0, 0
	for _, name := range attachments {
		block, kind, err := readAttachmentBlock(root, workspace, name)
		if err != nil {
			return nil, fmt.Errorf("附件 %q: %w", name, err)
		}
		if block == nil {
			continue
		}
		switch kind {
		case mediaImage:
			// base64 解出来的字节数才是真正进请求体的量（Data 是编码后的），
			// 拿 len(Data) 去比会把 4/3 的膨胀算漏。
			padding := len(block.Data) - len(strings.TrimRight(block.Data, "="))
			imageTotal += base64.StdEncoding.DecodedLen(len(block.Data)) - padding
			if len(blocks) >= maxImages || imageTotal > maxImageTotal {
				return nil, fmt.Errorf("图片最多4张，总大小不能超过20MiB")
			}
		case mediaVideo:
			videos++
			if videos > maxVideos {
				return nil, fmt.Errorf("一次最多只能发送1段视频")
			}
		}
		blocks = append(blocks, *block)
	}
	return blocks, nil
}

// mediaKind 是附件被识别出的模态。
type mediaKind int

const (
	mediaNone mediaKind = iota // 不是模型能吃的媒体（纯文本等），不产出内容块
	mediaImage
	mediaVideo
)

// readAttachmentBlock 识别单个附件并读成内容块。
//
// 返回 block=nil 表示「不是图片也不是视频」—— 那不是错误：
// 用户 @ 一个 .go / .md 文件是正常用法，模型走 read_file 自己去读，
// 这里不产出任何块（见 file_ops.go 的多格式文档提取）。
func readAttachmentBlock(root *os.Root, workspace, name string) (*llm.ContentBlock, mediaKind, error) {
	rel, err := attachmentRelative(root, workspace, name)
	if err != nil {
		return nil, mediaNone, err
	}
	info, err := root.Stat(rel)
	if err != nil {
		return nil, mediaNone, err
	}
	if !info.Mode().IsRegular() {
		return nil, mediaNone, fmt.Errorf("附件必须是普通文件")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, mediaNone, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, mediaNone, err
	}
	if !info.Mode().IsRegular() {
		// 竞态：Stat 时还是文件，打开时已换成目录/软链。不认。
		return nil, mediaNone, fmt.Errorf("附件必须是普通文件")
	}
	header := make([]byte, 512)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, mediaNone, err
	}
	header = header[:n]

	// 视频先判：它的判据不依赖 http.DetectContentType（那只认得 mp4/webm），
	// 靠容器魔数 + 扩展名，且体积上限与图片完全不同，两者不能混进一条分支。
	if mediaType, ok := videoMediaType(header, name); ok {
		if info.Size() > maxVideoBytes {
			return nil, mediaNone, fmt.Errorf("每段视频不能超过20MiB")
		}
		data, err := readLimited(file, header, maxVideoBytes)
		if err != nil {
			return nil, mediaNone, err
		}
		if len(data) > maxVideoBytes {
			return nil, mediaNone, fmt.Errorf("每段视频不能超过20MiB")
		}
		return &llm.ContentBlock{
			Type:      llm.BlockVideo,
			MediaType: mediaType,
			Data:      base64.StdEncoding.EncodeToString(data),
		}, mediaVideo, nil
	}

	mediaType := http.DetectContentType(header)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		switch strings.ToLower(filepath.Ext(name)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			return nil, mediaNone, fmt.Errorf("图片内容无效或已损坏")
		}
		return nil, mediaNone, nil
	}
	if info.Size() > maxImageBytes {
		return nil, mediaNone, fmt.Errorf("每张图片不能超过5MiB")
	}
	data, err := readLimited(file, header, maxImageBytes)
	if err != nil {
		return nil, mediaNone, err
	}
	if len(data) > maxImageBytes {
		return nil, mediaNone, fmt.Errorf("每张图片不能超过5MiB")
	}
	if err := validateAttachmentImage(data, mediaType); err != nil {
		return nil, mediaNone, err
	}
	return &llm.ContentBlock{Type: llm.BlockImage, MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}, mediaImage, nil
}

// readLimited 读整个文件并施加硬上限，超限时返回的长度会超过 limit（由调用方判定）。
//
// 半截视频/图片被当成完整内容送上去，是最坏的一种错：模型会对着
// 损坏的数据认真作答，界面上看不出任何异常。
func readLimited(file *os.File, header []byte, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(header), file), limit+1))
}

// videoMediaType 判定是不是视频，并给出要送上游的 MIME。
//
// 判据是**容器魔数 + 扩展名的交叉验证**，不是「扩展名像视频就算」：
// 只信扩展名会把任意二进制（改个 .mp4 后缀就）标成 video/mp4 送上去，
// 上游要么报错、要么对着一段垃圾给出言之凿凿的描述。
//
// 为什么不直接用 http.DetectContentType：它只认得 mp4（ftyp）与 webm（EBML），
// mov / mkv / avi 一律落到 application/octet-stream。而这些容器在
// Gemini 支持的格式列表里（mov / avi / webm / mpeg / 3gpp 都在）。
func videoMediaType(head []byte, name string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	want, known := videoExtTypes[ext]
	if !known {
		// 扩展名不在列表里就不猜：靠魔数救不了 file:/// 这类无扩展名路径，
		// 而猜错的代价（把垃圾当视频送出去）远大于漏掉一个冷门容器。
		return "", false
	}
	if len(head) < 12 {
		return "", false
	}
	if !videoContainerMagic(head) {
		return "", false
	}
	return want, true
}

// videoExtTypes 是按扩展名索引的视频 MIME。
//
// 键为含点的**小写**扩展名；值即送上游的 media_type（不要改成容器名，
// 上游按 media_type 选解码器）。
var videoExtTypes = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".qt":   "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpeg",
	".3gp":  "video/3gpp",
}

// videoContainerMagic 判断头部是否是已知视频容器的起始特征。
//
// 覆盖：ISO BMFF（mp4/mov/m4v 的 ftyp）、Matroska/WebM（EBML）、
// AVI（RIFF….AVI ）、MPEG-PS/TS（0x000001BA / 188 字节包的 0x47 同步字节）。
// 只做魔数判定，不校验内部结构 —— 完整校验需要真解码器，那是模型侧的事。
func videoContainerMagic(head []byte) bool {
	switch {
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}): // EBML：matroska / webm
		return true
	case len(head) >= 12 && string(head[4:8]) == "ftyp": // ISO BMFF：mp4 / mov
		return true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return true
	case bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xBA}): // MPEG-PS
		return true
	case len(head) >= 189 && head[0] == 0x47 && head[188] == 0x47: // MPEG-TS：相邻两包的首字节
		return true
	}
	return false
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
