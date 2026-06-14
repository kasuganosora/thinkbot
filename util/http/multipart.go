package http

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// ============================================================================
// MultipartForm — multipart/form-data 请求体构造器
//
// 用于文件上传和 multipart 表单提交。典型用法：
//
//	form := NewMultipartForm().
//	    AddFile("file", "report.pdf", strings.NewReader(data)).
//	    AddField("purpose", "vision")
//	resp, err := client.Post("/upload").SetMultipart(form).Do()
//
// MultipartForm 不是线程安全的。
// ============================================================================

// MultipartForm 封装 multipart.Writer，提供链式 API 构造 multipart/form-data 请求体。
type MultipartForm struct {
	buf    *bytes.Buffer
	writer *multipart.Writer
}

// NewMultipartForm 创建一个新的 multipart 表单构造器。
func NewMultipartForm() *MultipartForm {
	buf := &bytes.Buffer{}
	return &MultipartForm{
		buf:    buf,
		writer: multipart.NewWriter(buf),
	}
}

// AddField 添加一个普通文本字段。
func (f *MultipartForm) AddField(name, value string) *MultipartForm {
	_ = f.writer.WriteField(name, value)
	return f
}

// AddFile 添加一个文件字段，MIME 类型默认为 application/octet-stream。
func (f *MultipartForm) AddFile(name, filename string, reader io.Reader) *MultipartForm {
	part, err := f.writer.CreateFormFile(name, filename)
	if err != nil {
		return f // bytes.Buffer 不会出错
	}
	_, _ = io.Copy(part, reader)
	return f
}

// AddFileWithMIME 添加一个文件字段并指定 MIME 类型。
func (f *MultipartForm) AddFileWithMIME(name, filename, mimeType string, reader io.Reader) *MultipartForm {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name=%q; filename=%q`,
			escapeMultipartQuotes(name), escapeMultipartQuotes(filename)))
	if mimeType != "" {
		h.Set("Content-Type", mimeType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	part, err := f.writer.CreatePart(h)
	if err != nil {
		return f
	}
	_, _ = io.Copy(part, reader)
	return f
}

// ContentType 返回正确的 Content-Type 头值（包含 boundary）。
// 必须在 SetMultipart 之前调用，或直接由 SetMultipart 自动设置。
func (f *MultipartForm) ContentType() string {
	return f.writer.FormDataContentType()
}

// close 关闭 multipart writer，写入结束边界。
// bytes.Buffer 的 Write 永不返回错误，因此 Close 也不会出错。
func (f *MultipartForm) close() {
	_ = f.writer.Close()
}

// Bytes 返回已写入的字节（需先 close）。
func (f *MultipartForm) bytes() []byte {
	return f.buf.Bytes()
}

// escapeMultipartQuotes 转义 multipart 头值中的双引号和反斜杠。
func escapeMultipartQuotes(s string) string {
	return multipartQuoteEscaper.Replace(s)
}

var multipartQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
