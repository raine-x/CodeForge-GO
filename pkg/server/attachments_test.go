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

type attachmentRequestProvider struct {
	requests chan llm.Request
	err      error
}

func (p *attachmentRequestProvider) Name() string { return "text-only-model" }

func (p *attachmentRequestProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	p.requests <- req
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "ok"}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
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
	}{
		{name: "image-only"},
		{name: "mixed", text: "read @notes.txt and @photo.png"},
		{name: "invalid", invalid: true},
		{name: "auto-escape", invalid: true, escape: true},
		{name: "workspace-snapshot", snapshot: true},
		{name: "upstream", upstream: errors.New("upstream: this model does not support image input")},
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
				if !strings.Contains(strings.Join(errorsSeen, "\n"), tc.upstream.Error()) {
					t.Fatalf("upstream error lost: %v", errorsSeen)
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

func TestAttachmentWebPSignatures(t *testing.T) {
	root := t.TempDir()
	data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	putAttachment(t, root, "image.bin", data)
	images, err := readAttachmentImages(root, []string{"image.bin"})
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
			images, err := readAttachmentImages(root, []string{path})
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
		if _, err := readAttachmentImages(root, []string{tc.name}); err == nil {
			t.Errorf("accepted %s", tc.name)
		}
	}
	putAttachment(t, root, "notes.txt", []byte("plain text"))
	images, err := readAttachmentImages(root, []string{"notes.txt"})
	if err != nil || len(images) != 0 {
		t.Fatalf("non-image: %v %+v", err, images)
	}
	jpegData := attachmentFixture(t, "jpeg")
	jpegData = append(jpegData, make([]byte, maxImageBytes-len(jpegData))...)
	putAttachment(t, root, "limit.jpg", jpegData)
	images, err = readAttachmentImages(root, []string{"limit.jpg", "limit.jpg", "limit.jpg", "limit.jpg"})
	if err != nil || len(images) != 4 {
		t.Fatalf("exact total: %v, %d", err, len(images))
	}
	if _, err := readAttachmentImages(root, []string{"image-png.bin", "image-png.bin", "image-png.bin", "image-png.bin", "image-png.bin"}); err == nil {
		t.Fatal("accepted five images")
	}
}

func TestAttachmentPathBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := putAttachment(t, t.TempDir(), "outside.png", attachmentFixture(t, "png"))
	for _, name := range []string{outside, "../outside.png", `..\outside.png`, "https://example.invalid/a.png", "file:///a.png", ".", ""} {
		if _, err := readAttachmentImages(root, []string{name}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := readAttachmentImages("", []string{"a.png"}); err == nil {
		t.Fatal("accepted no workspace")
	}
	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(root, "linked.png")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := readAttachmentImages(root, []string{"linked.png"}); err == nil {
			t.Fatal("accepted escaping symlink")
		}
		inside := putAttachment(t, root, "inside.png", attachmentFixture(t, "png"))
		if err := os.Symlink(inside, filepath.Join(root, "safe.png")); err != nil {
			t.Fatal(err)
		}
		if _, err := readAttachmentImages(root, []string{"safe.png"}); err != nil {
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
