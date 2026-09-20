package builtin

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// 构造一个最小可用的 docx（zip 包含 word/document.xml）。
func buildDocx(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` +
		body + `</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDocx(t *testing.T) {
	data := buildDocx(t, "绣球项目需求文档")
	text, ok, err := extractDocText(data, "需求.docx")
	if !ok {
		t.Fatal("docx 应被识别为支持的格式")
	}
	if err != nil {
		t.Fatalf("docx 提取失败: %v", err)
	}
	if !strings.Contains(text, "绣球项目需求文档") {
		t.Errorf("提取结果缺少正文，实际: %q", text)
	}
}

func TestExtractDocxInvalid(t *testing.T) {
	// 非 zip 内容挂 .docx 后缀：应返回错误而非乱码。
	_, ok, err := extractDocText([]byte("not a zip"), "x.docx")
	if !ok || err == nil {
		t.Errorf("损坏 docx 应报错，ok=%v err=%v", ok, err)
	}
}

func TestExtractPdf(t *testing.T) {
	// 最小文本型 PDF：内容流里有 Tj 操作的字面字符串。
	pdf := "%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n" +
		"stream\nBT /F1 12 Tf (Hello CodeForge PDF) Tj ET\nendstream\n%%EOF"
	text, ok, err := extractDocText([]byte(pdf), "doc.pdf")
	if !ok {
		t.Fatal("pdf 应被识别")
	}
	if err != nil {
		t.Fatalf("pdf 提取失败: %v", err)
	}
	if !strings.Contains(text, "Hello CodeForge PDF") {
		t.Errorf("pdf 提取缺少文本，实际: %q", text)
	}
}

func TestExtractPdfScanned(t *testing.T) {
	// 没有任何字面字符串的「扫描件」PDF：应给出明确错误而非空串。
	pdf := "%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF"
	_, ok, err := extractDocText([]byte(pdf), "scan.pdf")
	if !ok || err == nil {
		t.Errorf("扫描件 PDF 应报「无法提取」，ok=%v err=%v", ok, err)
	}
}

func TestExtractUnsupported(t *testing.T) {
	_, ok, err := extractDocText([]byte("plain"), "note.txt")
	if ok || err != nil {
		t.Errorf("txt 不属于提取分支，应原样走文本路径，ok=%v err=%v", ok, err)
	}
}

func TestExtractPptx(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ppt/slides/slide1.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>第一页标题</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	text, ok, err := extractDocText(buf.Bytes(), "deck.pptx")
	if !ok || err != nil {
		t.Fatalf("pptx 提取失败 ok=%v err=%v", ok, err)
	}
	if !strings.Contains(text, "第一页标题") || !strings.Contains(text, "幻灯片 1") {
		t.Errorf("pptx 提取结果异常: %q", text)
	}
}

// pdfLiteral 的嵌套括号与转义。
func TestPdfLiteral(t *testing.T) {
	s, n := pdfLiteral([]byte("(a(b)c) rest"))
	if s != "a(b)c" || n != len("(a(b)c)") {
		t.Errorf("嵌套括号解析错误: s=%q n=%d", s, n)
	}
	s, _ = pdfLiteral([]byte(`(a\nb\(c\)) x`))
	if !strings.Contains(s, "a") {
		t.Errorf("转义解析错误: %q", s)
	}
	if _, n := pdfLiteral([]byte("(unclosed")); n != 0 {
		t.Errorf("未闭合应返回 0，实际 %d", n)
	}
}
