package builtin

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// doc_extract.go 为 read_file 提供「多格式文件 → 纯文本」的提取能力。
//
// 覆盖两类：
//   - Office Open XML（.docx / .pptx / .xlsx）：本质是 zip 包 + XML，
//     用标准库 archive/zip 解开、抽出文本节点，零新增依赖；
//   - .pdf：轻量文本流抽取（BT…ET 之间的 Tj/TJ 操作数），只覆盖文本型 PDF。
//
// 视频走第三条路：明确报「读不了」并给出替代做法（见那个分支的注释）。
//
// 设计取向：不追求排版还原，只把「正文文字」喂给模型 —— Agent 要的是内容，
// 不是版式。需要精确版式时请用户转 Markdown / 纯文本。

// extractDocText 按扩展名分派提取；不支持的类型返回 ("", false, nil)。
func extractDocText(data []byte, name string) (string, bool, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".docx":
		t, err := docxText(data)
		return t, true, err
	case ".pptx":
		t, err := pptxText(data)
		return t, true, err
	case ".xlsx":
		t, err := xlsxText(data)
		return t, true, err
	case ".pdf":
		t, err := pdfText(data)
		return t, true, err
	case ".mp4", ".m4v", ".webm", ".mov", ".qt", ".mkv", ".avi",
		".mpeg", ".mpg", ".3gp":
		// 视频明确报「读不了」，而不是让它落到下面 `string(data)` 那一步。
		//
		// 后者会把整段二进制按行切开返回给模型：一屏乱码，
		// 而模型会**认真地**照着乱码编出一段描述。此前附件层对视频
		// 静默返回 nil（连一个块都不给），模型只能自己去 read_file，
		// 于是走的正是这条吐乱码的路。
		return "", true, fmt.Errorf(
			"视频文件无法按文本读取（需要真正的解码器才能取出画面与声音）。" +
				"请改用随对话附件发送：支持视频的模型会直接解析它；" +
				"不支持的模型请自行抽取关键帧后以图片形式附上")
	}
	return "", false, nil
}

// ---------------------------------------------------------------------------
// OOXML（docx / pptx / xlsx）：zip + XML 文本节点抽取
// ---------------------------------------------------------------------------

func openZip(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

// readZipXML 解开包里指定成员并抽出全部 XML 字符数据（Chardata），
// 以空格连接。OOXML 的正文就散落在各 <w:t> / <a:t> 节点里。
func readZipXML(zr *zip.Reader, member string) (string, error) {
	f, err := zr.Open(member)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return xmlCharData(f)
}

// xmlCharData 流式读取 XML，拼接所有字符数据节点。
func xmlCharData(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sb.String(), err
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), nil
}

func zipHas(zr *zip.Reader, name string) bool {
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

// zipMembers 返回匹配前缀与后缀的全部成员名（按包内顺序）。
func zipMembers(zr *zip.Reader, prefix, suffix string) []string {
	var out []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, suffix) {
			out = append(out, f.Name)
		}
	}
	return out
}

// docxText 提取 Word 正文（word/document.xml）。
func docxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("不是有效的 docx（zip 打开失败）: %v", err)
	}
	if !zipHas(zr, "word/document.xml") {
		return "", fmt.Errorf("不是有效的 docx（缺少 word/document.xml）")
	}
	text, err := readZipXML(zr, "word/document.xml")
	if err != nil {
		return "", fmt.Errorf("docx 正文解析失败: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("docx 中未提取到文本（可能是空文档或扫描件）")
	}
	return text, nil
}

// pptxText 提取 PPT 全部幻灯片文字（ppt/slides/slide*.xml）。
func pptxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("不是有效的 pptx（zip 打开失败）: %v", err)
	}
	slides := zipMembers(zr, "ppt/slides/slide", ".xml")
	if len(slides) == 0 {
		return "", fmt.Errorf("不是有效的 pptx（没有幻灯片）")
	}
	var sb strings.Builder
	for i, m := range slides {
		t, err := readZipXML(zr, m)
		if err != nil || strings.TrimSpace(t) == "" {
			continue // 单页损坏或空白不拖垮整体
		}
		fmt.Fprintf(&sb, "—— 幻灯片 %d ——\n%s\n\n", i+1, t)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("pptx 中未提取到文本")
	}
	return sb.String(), nil
}

// xlsxText 提取 Excel 文本：共享字符串表（xl/sharedStrings.xml）。
// xlsx 的文字大多集中在共享字符串表里；工作表里多是索引与数字。
// 抽出共享字符串已足够让模型理解表格内容。
func xlsxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("不是有效的 xlsx（zip 打开失败）: %v", err)
	}
	if !zipHas(zr, "xl/sharedStrings.xml") {
		return "", fmt.Errorf("xlsx 中没有共享字符串表（可能全为数字或空表）")
	}
	text, err := readZipXML(zr, "xl/sharedStrings.xml")
	if err != nil {
		return "", fmt.Errorf("xlsx 解析失败: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("xlsx 中未提取到文本")
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// PDF：轻量文本流抽取
// ---------------------------------------------------------------------------

// pdfText 从 PDF 内容流中抽取 Tj / TJ 操作符的字符串操作数。
//
// 这是刻意保持轻量的实现：纯 Go、零依赖，覆盖「文本型 PDF」的常见情形。
// 已知边界（如实告知，不编造）：扫描件（整页是图片）提取为空；使用自定义
// 字体编码（CID/ToUnicode 缺失）时可能得到乱码；不做布局还原。
func pdfText(data []byte) (string, error) {
	if len(data) < 5 || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return "", fmt.Errorf("不是有效的 PDF 文件")
	}
	var sb strings.Builder
	for i := 0; i < len(data); i++ {
		if data[i] != '(' {
			continue
		}
		s, n := pdfLiteral(data[i:])
		if n == 0 {
			continue
		}
		i += n - 1
		if len(strings.TrimSpace(s)) > 0 && printableRatio(s) > 0.6 {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(s)
		}
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("未能从 PDF 提取文本（可能是扫描件/图片型 PDF，或使用了嵌入字体编码）")
	}
	return out, nil
}

// pdfLiteral 解析一个以 '(' 开头的 PDF 字面字符串，返回内容与消费的字节数。
func pdfLiteral(b []byte) (string, int) {
	if len(b) == 0 || b[0] != '(' {
		return "", 0
	}
	depth := 0
	var sb strings.Builder
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\\' && i+1 < len(b) {
			nx := b[i+1]
			switch nx {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(nx)
			}
			i++
			continue
		}
		if c == '(' {
			depth++
			if depth > 1 {
				sb.WriteByte(c)
			}
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return sb.String(), i + 1
			}
			sb.WriteByte(c)
			continue
		}
		if depth >= 1 {
			sb.WriteByte(c)
		}
	}
	return "", 0
}

// printableRatio 估算「可打印文本」占比，用于过滤内容流里混进的二进制片段。
func printableRatio(s string) float64 {
	if s == "" {
		return 0
	}
	printable, total := 0, 0
	for _, r := range s {
		total++
		if r == ' ' || r == '\n' || r == '\t' || (r >= 0x20 && r < 0x7f) || r >= 0x80 {
			printable++
		}
	}
	return float64(printable) / float64(total)
}
