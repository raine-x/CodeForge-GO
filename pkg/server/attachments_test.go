package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/llm"
	"github.com/gorilla/websocket"
)

func attachmentFixture(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func putAttachment(t *testing.T, root, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// videoFixture 造一段带真实容器魔数的最小视频数据。
//
// 只写魔数不写真实码流是刻意的：本层只做**容器识别**（魔数 + 扩展名），
// 完整解码是模型侧的事（见 agent/capability.go）。测试要钉的是
// 「这段数据会被认成视频」与「不是视频的不会被误认」这两件事。
func videoFixture(head []byte, size int) []byte {
	buf := make([]byte, size)
	copy(buf, head)
	for i := len(head); i < size; i++ {
		buf[i] = byte(i % 251)
	}
	return buf
}

// 各类容器的起始字节。ftyp 放在 offset 4（ISO BMFF 的固定位置）。
var (
	mp4Head  = append([]byte{0x00, 0x00, 0x00, 0x18}, []byte("ftypisom")...)
	webmHead = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("\x01\x00\x00\x00\x00\x00\x00\x1f")...)
	aviHead  = append([]byte("RIFF"), append([]byte{0x00, 0x00, 0x00, 0x00}, []byte("AVI ")...)...)
	mkvHead  = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("\x42\x82\x81\x01\x42\x86\x81\x01")...)
	psHead   = []byte{0x00, 0x00, 0x01, 0xBA}
)

type attachmentRequestProvider struct {
	requests chan llm.Request
	err      error
}

func (p *attachmentRequestProvider) Name() string { return "text-only-model" }

func (p *attachmentRequestProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	p.requests <- cloneRequest(req)
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "ok"}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
}

// cloneRequest 深拷贝一份请求再存起来。
//
// 不能直接存 req：送模视图与 Session 历史**共享 Message 结构体**，
// 所以上游拒收媒体后的「摘掉历史里的媒体块」会就地改到这份引用上 ——
// 测试读到的是回滚之后的状态，而不是真正发出去的那一份。
func cloneRequest(req llm.Request) llm.Request {
	out := req
	out.Messages = make([]llm.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		blocks := make([]llm.ContentBlock, len(m.Content))
		copy(blocks, m.Content)
		m.Content = blocks
		out.Messages = append(out.Messages, m)
	}
	return out
}

type attachmentWSWriter struct {
	*httptest.ResponseRecorder
	conn   net.Conn
	reader *bufio.Reader
}

func (w *attachmentWSWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(w.reader, bufio.NewWriter(w.conn)), nil
}

func attachmentWS(t *testing.T, srv *Server) *websocket.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		reader := bufio.NewReader(server)
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		srv.Routes().ServeHTTP(&attachmentWSWriter{httptest.NewRecorder(), server, reader}, req)
	}()
	dialer := websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }, HandshakeTimeout: 5 * time.Second}
	header := http.Header{}
	header.Set("Cookie", TokenCookie+"="+srv.token)
	conn, _, err := dialer.Dial("ws://localhost/ws", header)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("ready = %v, %v", ready, err)
	}
	return conn
}

func TestAttachmentWSImagesAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		invalid  bool
		escape   bool
		snapshot bool
		upstream error
		// wantError 非空时，断言界面收到的是**这段**说明；
		// wantNoRawErr 非空时，断言界面**不含**上游原文。
		wantError    string
		wantNoRawErr string
	}{
		{name: "image-only"},
		{name: "mixed", text: "read @notes.txt and @photo.png"},
		{name: "invalid", invalid: true},
		{name: "auto-escape", invalid: true, escape: true},
		{name: "workspace-snapshot", snapshot: true},
		{
			// 上游拒收图片：界面上必须是人话，不能是上游那串英文原文。
			// 此前这条断言守的是「原文透传」，那正是本次要消掉的行为。
			name:         "upstream",
			upstream:     errors.New("upstream: this model does not support image input"),
			wantError:    "不支持接收图片/视频附件",
			wantNoRawErr: "this model does not support image input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &attachmentRequestProvider{requests: make(chan llm.Request, 2), err: tc.upstream}
			d := newTestDepsAtProvider(t, "", p)
			off := false
			d.cfg.Notify.Enabled = &off
			data := attachmentFixture(t, "png")
			if tc.invalid {
				data = []byte("not an image")
			}
			putAttachment(t, d.dir, "photo.png", data)
			putAttachment(t, d.dir, "notes.txt", []byte("local notes"))
			paths := []string{"notes.txt", "photo.png"}
			if tc.escape {
				d.executor.Policy().SetMode("auto")
				paths = []string{putAttachment(t, t.TempDir(), "outside.png", attachmentFixture(t, "png"))}
			}
			other := t.TempDir()
			conn := attachmentWS(t, d.newServer())
			if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": tc.text, "attachments": paths}); err != nil {
				t.Fatal(err)
			}
			var errorsSeen []string
			busy, idle := false, false
			for i := 0; i < 40; i++ {
				var ev map[string]any
				if err := conn.ReadJSON(&ev); err != nil {
					t.Fatal(err)
				}
				if ev["type"] == "session" && tc.snapshot {
					d.fsys.SetRoot(other)
					d.agent.SetWorkDir(other)
				}
				if ev["type"] == "busy" {
					busy = true
				}
				if ev["type"] == "idle" {
					idle = true
				}
				if ev["type"] == "error" {
					errorsSeen = append(errorsSeen, ev["error"].(string))
				}
				if ev["type"] == "context" && idle {
					break
				}
			}
			if !busy || !idle {
				t.Fatal("missing busy/idle")
			}
			if tc.invalid {
				if len(errorsSeen) == 0 || len(p.requests) != 0 {
					t.Fatal("invalid image reached provider or error missing")
				}
				return
			}
			select {
			case req := <-p.requests:
				last := req.Messages[len(req.Messages)-1]
				if last.Role != llm.RoleUser || len(last.Content) != 2 || last.Content[0].Text != tc.text {
					t.Fatalf("unexpected message: %+v", last)
				}
				block := last.Content[1]
				if block.Type != llm.BlockImage || block.MediaType != "image/png" || block.Data != base64.StdEncoding.EncodeToString(data) {
					t.Fatalf("wrong image block: %+v", block)
				}
			default:
				t.Fatal("image did not reach provider")
			}
			if tc.upstream != nil {
				got := strings.Join(errorsSeen, "\n")
				if tc.wantError != "" && !strings.Contains(got, tc.wantError) {
					t.Fatalf("界面错误应说明原因（%q），实际: %v", tc.wantError, errorsSeen)
				}
				if tc.wantNoRawErr != "" && strings.Contains(got, tc.wantNoRawErr) {
					t.Fatalf("上游原文不该透给用户（%q 出现在 %v 里）", tc.wantNoRawErr, errorsSeen)
				}
			} else if len(errorsSeen) != 0 {
				t.Fatalf("unexpected errors: %v", errorsSeen)
			}
		})
	}
}

func uploadRequest(t *testing.T, srv *Server, names []string, data []byte, auth bool) (*http.Request, []byte) {
	t.Helper()
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	for _, name := range names {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/upload_file", bytes.NewReader(b.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if auth {
		req.AddCookie(&http.Cookie{Name: TokenCookie, Value: srv.token})
	}
	return req, b.Bytes()
}

func TestUploadAttachmentHTTP(t *testing.T) {
	d := newTestDeps(t)
	srv := d.newServer()
	routes := srv.Routes()
	data := []byte("streamed contents")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		req, _ := uploadRequest(t, srv, []string{"原文件.txt"}, data, true)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		var got struct {
			OK   bool   `json:"ok"`
			Path string `json:"staged_path"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || !got.OK || got.Name != "原文件.txt" || !strings.HasPrefix(got.Path, "attachments/") || seen[got.Path] {
			t.Fatalf("response: %d %s", w.Code, w.Body)
		}
		seen[got.Path] = true
		stored, err := os.ReadFile(filepath.Join(d.dir, filepath.FromSlash(got.Path)))
		if err != nil || !bytes.Equal(stored, data) {
			t.Fatalf("stored data: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		method string
		auth   bool
		names  []string
		status int
	}{
		{"auth", "POST", false, []string{"a.txt"}, 401},
		{"method", "GET", true, []string{"a.txt"}, 405},
		{"missing", "POST", true, nil, 400},
		{"duplicate", "POST", true, []string{"a.txt", "b.txt"}, 400},
		{"traversal", "POST", true, []string{"../escape.txt"}, 400},
		{"windows-traversal", "POST", true, []string{`..\escape.txt`}, 400},
		{"absolute", "POST", true, []string{"/escape.txt"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := uploadRequest(t, srv, tc.names, data, tc.auth)
			req.Method = tc.method
			w := httptest.NewRecorder()
			routes.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			entries, err := os.ReadDir(filepath.Join(d.dir, "attachments"))
			if err != nil || len(entries) != 2 {
				t.Fatalf("failed upload left files: %v %v", entries, err)
			}
		})
	}
	d.fsys.SetRoot("")
	req, _ := uploadRequest(t, srv, []string{"a.txt"}, data, true)
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("no workspace: %d", w.Code)
	}
}

func TestUploadAttachmentLimitsAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		trailing int
		truncate bool
		want     int
	}{
		{"exact", maxUploadFile, 0, false, 200},
		{"file-over", maxUploadFile + 1, 0, false, 413},
		{"body-over", 1, maxUploadBody, false, 413},
		{"truncated", 128, 0, true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDeps(t)
			srv := d.newServer()
			req, body := uploadRequest(t, srv, []string{"a.bin"}, bytes.Repeat([]byte("x"), tc.size), true)
			if tc.truncate {
				body = body[:len(body)-12]
			}
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), io.LimitReader(&attachmentZeroReader{}, int64(tc.trailing))))
			req.ContentLength = -1
			w := httptest.NewRecorder()
			srv.Routes().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			entries, err := os.ReadDir(filepath.Join(d.dir, "attachments"))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.want == 200 {
				want = 1
			}
			if len(entries) != want {
				t.Fatalf("left files: %v", entries)
			}
		})
	}
}

type attachmentZeroReader struct{}

func (*attachmentZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type attachmentSwitchReader struct {
	io.Reader
	switchRoot func()
}

func (r *attachmentSwitchReader) Read(p []byte) (int, error) {
	if r.switchRoot != nil {
		r.switchRoot()
		r.switchRoot = nil
	}
	return r.Reader.Read(p)
}

func TestUploadAttachmentWorkspaceSnapshot(t *testing.T) {
	d := newTestDeps(t)
	srv := d.newServer()
	other := t.TempDir()
	req, body := uploadRequest(t, srv, []string{"a.txt"}, []byte("snapshot"), true)
	req.Body = io.NopCloser(&attachmentSwitchReader{Reader: bytes.NewReader(body), switchRoot: func() {
		d.fsys.SetRoot(other)
		d.agent.SetWorkDir(other)
	}})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	entries, err := os.ReadDir(filepath.Join(d.dir, "attachments"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot target: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(other, "attachments")); !os.IsNotExist(err) {
		t.Fatalf("new workspace modified: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 视频识别
// ---------------------------------------------------------------------------

// 各容器都应被认成视频，并带上正确的 media_type。
func TestAttachmentVideoContainers(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		head []byte
		want string
	}{
		{"clip.mp4", mp4Head, "video/mp4"},
		{"clip.m4v", mp4Head, "video/mp4"},
		{"clip.mov", mp4Head, "video/quicktime"},
		{"clip.webm", webmHead, "video/webm"},
		{"clip.mkv", mkvHead, "video/x-matroska"},
		{"clip.avi", aviHead, "video/x-msvideo"},
		{"clip.mpg", psHead, "video/mpeg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := videoFixture(tc.head, 4096)
			putAttachment(t, root, tc.name, data)
			blocks, err := readAttachmentMedia(root, []string{tc.name})
			if err != nil {
				t.Fatalf("识别视频失败: %v", err)
			}
			if len(blocks) != 1 {
				t.Fatalf("应产出 1 个内容块，实际 %d", len(blocks))
			}
			if blocks[0].Type != llm.BlockVideo {
				t.Errorf("块类型应为 video，实际 %q", blocks[0].Type)
			}
			if blocks[0].MediaType != tc.want {
				t.Errorf("media_type 应为 %q，实际 %q", tc.want, blocks[0].MediaType)
			}
			if blocks[0].Data != base64.StdEncoding.EncodeToString(data) {
				t.Error("视频内容未被完整送达（base64 不匹配）")
			}
		})
	}
}

// 只信扩展名会把任意二进制标成视频送上去 —— 上游要么报错，
// 要么对着一段垃圾给出言之凿凿的描述。这两种都比报错糟。
func TestAttachmentVideoRejectsNonVideoContent(t *testing.T) {
	root := t.TempDir()
	cases := map[string][]byte{
		// 改了后缀的文本文件：没有容器魔数
		"fake.mp4":    append([]byte("not a video at all, just text padding"), bytes.Repeat([]byte{0x41}, 2048)...),
		"script.webm": append([]byte("#!/bin/sh\necho hi\n"), bytes.Repeat([]byte{0x0A}, 2048)...),
	}
	for name, data := range cases {
		putAttachment(t, root, name, data)
		blocks, err := readAttachmentMedia(root, []string{name})
		if err != nil {
			t.Fatalf("%s 不该报错（它只是普通文件，应交给 read_file）: %v", name, err)
		}
		if len(blocks) != 0 {
			t.Errorf("%s 内容不是视频，不该产出内容块，实际 %+v", name, blocks)
		}
	}
}

// 扩展名不在白名单里就不猜：宁可漏掉冷门容器，也不把垃圾送上去。
func TestAttachmentVideoUnknownExtensionIsNotGuessed(t *testing.T) {
	root := t.TempDir()
	putAttachment(t, root, "clip.bin", videoFixture(mp4Head, 2048))
	blocks, err := readAttachmentMedia(root, []string{"clip.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Errorf("无扩展名/未知扩展名不该被猜成视频，实际 %+v", blocks)
	}
}

// 一轮最多一段视频：视频的 token 开销是按字节估的（见 agent 的 videoTokens），
// 多段会直接把上下文顶爆，而用户往往没意识到视频有代价。
func TestAttachmentVideoCountLimit(t *testing.T) {
	root := t.TempDir()
	a := putAttachment(t, root, "a.mp4", videoFixture(mp4Head, 2048))
	b := putAttachment(t, root, "b.mp4", videoFixture(mp4Head, 2048))
	if _, err := readAttachmentMedia(root, []string{a, b}); err == nil {
		t.Fatal("两段视频应被拒绝")
	}
}

// 视频与图片各有独立预算：20MiB 的视频不该因为「图片总大小 20MiB」
// 这条图片专用规则而被拒 —— 那是两回事。
func TestAttachmentVideoHasItsOwnBudget(t *testing.T) {
	root := t.TempDir()
	putAttachment(t, root, "big.mp4", videoFixture(mp4Head, maxVideoBytes))
	blocks, err := readAttachmentMedia(root, []string{"big.mp4"})
	if err != nil {
		t.Fatalf("等于上限的视频应被接受: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("应产出 1 个块，实际 %d", len(blocks))
	}
	// 超上限的必须被拒。
	putAttachment(t, root, "over.mp4", videoFixture(mp4Head, maxVideoBytes+1))
	if _, err := readAttachmentMedia(root, []string{"over.mp4"}); err == nil {
		t.Error("超限视频应被拒绝")
	}
}

// 图片与视频可以同轮共存，且各自计数。
func TestAttachmentImageAndVideoCoexist(t *testing.T) {
	root := t.TempDir()
	png := putAttachment(t, root, "a.png", attachmentFixture(t, "png"))
	vid := putAttachment(t, root, "a.mp4", videoFixture(mp4Head, 2048))
	blocks, err := readAttachmentMedia(root, []string{png, vid})
	if err != nil {
		t.Fatalf("图片与视频同轮应被接受: %v", err)
	}
	kinds := map[string]bool{}
	for _, b := range blocks {
		kinds[b.Type] = true
	}
	if !kinds[llm.BlockImage] || !kinds[llm.BlockVideo] {
		t.Errorf("两种块都该存在，实际 %v", kinds)
	}
}

func TestAttachmentWebPSignatures(t *testing.T) {
	root := t.TempDir()
	data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	putAttachment(t, root, "image.bin", data)
	images, err := readAttachmentMedia(root, []string{"image.bin"})
	if err != nil || len(images) != 1 || images[0].MediaType != "image/webp" {
		t.Fatalf("WebP: %v %+v", err, images)
	}
	for n := 0; n < len(data); n++ {
		if err := validateAttachmentImage(data[:n], "image/webp"); err == nil {
			t.Fatalf("accepted truncated WebP: %d", n)
		}
	}
}

func TestAttachmentImageValidation(t *testing.T) {
	root := t.TempDir()
	for _, format := range []string{"png", "jpeg", "gif"} {
		data := attachmentFixture(t, format)
		name := "image-" + format + ".bin"
		p := putAttachment(t, root, name, data)
		for _, path := range []string{name, p} {
			images, err := readAttachmentMedia(root, []string{path})
			if err != nil || len(images) != 1 || images[0].MediaType != "image/"+format || images[0].Data != base64.StdEncoding.EncodeToString(data) {
				t.Fatalf("%s: %v %+v", path, err, images)
			}
		}
	}
	pngData := attachmentFixture(t, "png")
	huge := append([]byte(nil), pngData...)
	binary.BigEndian.PutUint32(huge[16:20], maxImagePixels+1)
	binary.BigEndian.PutUint32(huge[20:24], 1)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"fake.png", []byte("plain text")},
		{"fake.JPG", []byte("plain text")},
		{"broken.png", pngData[:len(pngData)-12]},
		{"huge.png", huge},
		{"oversize.png", append(pngData, make([]byte, maxImageBytes)...)},
		{"fake.webp", []byte("RIFFxxxxWEBPVP8 invalid")},
	} {
		putAttachment(t, root, tc.name, tc.data)
		if _, err := readAttachmentMedia(root, []string{tc.name}); err == nil {
			t.Errorf("accepted %s", tc.name)
		}
	}
	putAttachment(t, root, "notes.txt", []byte("plain text"))
	images, err := readAttachmentMedia(root, []string{"notes.txt"})
	if err != nil || len(images) != 0 {
		t.Fatalf("non-image: %v %+v", err, images)
	}
	jpegData := attachmentFixture(t, "jpeg")
	jpegData = append(jpegData, make([]byte, maxImageBytes-len(jpegData))...)
	putAttachment(t, root, "limit.jpg", jpegData)
	images, err = readAttachmentMedia(root, []string{"limit.jpg", "limit.jpg", "limit.jpg", "limit.jpg"})
	if err != nil || len(images) != 4 {
		t.Fatalf("exact total: %v, %d", err, len(images))
	}
	if _, err := readAttachmentMedia(root, []string{"image-png.bin", "image-png.bin", "image-png.bin", "image-png.bin", "image-png.bin"}); err == nil {
		t.Fatal("accepted five images")
	}
}

func TestAttachmentPathBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := putAttachment(t, t.TempDir(), "outside.png", attachmentFixture(t, "png"))
	for _, name := range []string{outside, "../outside.png", `..\outside.png`, "https://example.invalid/a.png", "file:///a.png", ".", ""} {
		if _, err := readAttachmentMedia(root, []string{name}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := readAttachmentMedia("", []string{"a.png"}); err == nil {
		t.Fatal("accepted no workspace")
	}
	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(root, "linked.png")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := readAttachmentMedia(root, []string{"linked.png"}); err == nil {
			t.Fatal("accepted escaping symlink")
		}
		inside := putAttachment(t, root, "inside.png", attachmentFixture(t, "png"))
		if err := os.Symlink(inside, filepath.Join(root, "safe.png")); err != nil {
			t.Fatal(err)
		}
		if _, err := readAttachmentMedia(root, []string{"safe.png"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUploadAttachmentSymlinkEscape(t *testing.T) {
	d := newTestDeps(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(d.dir, "attachments")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	srv := d.newServer()
	req, _ := uploadRequest(t, srv, []string{"a.txt"}, []byte("test"), true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d", w.Code)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}
