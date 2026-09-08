package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMultipartAddFileEscapesQuotes 锁定一个容易被误判的事实：
// AddFile 内部委托标准库 CreateFormFile，后者用 fmt.Sprintf("%q") 拼 header，
// 本身就会转义双引号与反斜杠。因此含引号的 filename 不会损坏
// Content-Disposition 头，服务端能完整还原原始文件名。
//
// 历史上有过"AddFile 不转义引号→会损坏 multipart 头"的误判，并据此去"统一"
// 调用点。本条测试防止该误判复发：只要 AddFile 不再正确转义，服务端解析就会失败
// 或文件名被截断，测试随即变红。
func TestMultipartAddFileEscapesQuotes(t *testing.T) {
	cases := []string{
		`re"port.txt`,
		`a\b.txt`,
		`normal.pdf`,
	}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(10 << 20); err != nil {
					t.Errorf("parse multipart: %v", err)
					return
				}
				_, header, err := r.FormFile("file")
				if err != nil {
					t.Errorf("form file: %v", err)
					return
				}
				if header.Filename != name {
					t.Errorf("round-trip filename mismatch: got %q want %q", header.Filename, name)
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			c := New(WithBaseURL(srv.URL))
			form := NewMultipartForm().
				AddFile("file", name, strings.NewReader("DATA"))
			resp, err := c.Post("/upload").SetMultipart(form).Do()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Errorf("expected 200, got %d", resp.StatusCode)
			}
		})
	}
}
