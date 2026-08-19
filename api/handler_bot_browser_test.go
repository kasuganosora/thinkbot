package api

import "testing"

// TestParseBrowserCookieImport_FormatRouting 覆盖四种导入格式的判定与解析，
// 重点验证新增的 Cookie header 格式不会误伤原有三种格式（格式判定是唯一的分派点）。
func TestParseBrowserCookieImport_FormatRouting(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		domain      string
		wantErr     bool
		wantCount   int
		wantFirstNm string
		wantFirstVl string
		wantFirstDm string
	}{
		{
			name:        "storageState JSON（浮点 expires）",
			raw:         `{"cookies":[{"name":"sid","value":"abc","domain":".x.com","path":"/","expires":1787132206.428}],"origins":[]}`,
			wantCount:   1,
			wantFirstNm: "sid",
			wantFirstVl: "abc",
			wantFirstDm: ".x.com",
		},
		{
			name:        "扩展导出 JSON 数组",
			raw:         `[{"name":"tok","value":"v1","domain":"a.com","path":"/","expirationDate":1799999999.5}]`,
			wantCount:   1,
			wantFirstNm: "tok",
			wantFirstVl: "v1",
			wantFirstDm: "a.com",
		},
		{
			name:        "Netscape cookies.txt",
			raw:         "# Netscape HTTP Cookie File\n.x.com\tTRUE\t/\tFALSE\t1799999999\tsid\tnetval",
			wantCount:   1,
			wantFirstNm: "sid",
			wantFirstVl: "netval",
			wantFirstDm: ".x.com",
		},
		{
			name:        "Cookie header 基本形态",
			raw:         "a=1; b=2; c=3",
			domain:      "example.com",
			wantCount:   3,
			wantFirstNm: "a",
			wantFirstVl: "1",
			wantFirstDm: "example.com",
		},
		{
			name:        "Cookie header 带字段名前缀",
			raw:         "Cookie: sid=xyz; other=2",
			domain:      "example.com",
			wantCount:   2,
			wantFirstNm: "sid",
			wantFirstVl: "xyz",
			wantFirstDm: "example.com",
		},
		{
			name:        "Cookie header 值内含等号与 base64 尾部",
			raw:         "token=aGVsbG8=; k2=a=b=c",
			domain:      "example.com",
			wantCount:   2,
			wantFirstNm: "token",
			wantFirstVl: "aGVsbG8=",
			wantFirstDm: "example.com",
		},
		{
			name:      "Cookie header 缺 domain 应报错",
			raw:       "a=1; b=2",
			domain:    "",
			wantErr:   true,
			wantCount: 0,
		},
		{
			name:        "Cookie header 跳过 Set-Cookie 属性关键字",
			raw:         "sid=abc; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=3600",
			domain:      "example.com",
			wantCount:   1,
			wantFirstNm: "sid",
			wantFirstVl: "abc",
			wantFirstDm: "example.com",
		},
		{
			name:        "Cookie header 同名后者覆盖前者",
			raw:         "dup=first; other=x; dup=second",
			domain:      "example.com",
			wantCount:   2,
			wantFirstNm: "dup",
			wantFirstVl: "second",
			wantFirstDm: "example.com",
		},
		{
			name:    "空 JSON 对象不应被当作 header",
			raw:     `{"cookies":[]}`,
			domain:  "example.com",
			wantErr: true,
		},
		{
			name:    "纯文本无 name=value 应报错",
			raw:     "hello world",
			domain:  "example.com",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBrowserCookieImport(tc.raw, tc.domain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功解析 %d 条", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误: %v", err)
			}
			if len(got) != tc.wantCount {
				t.Fatalf("条数不符: want %d, got %d (%+v)", tc.wantCount, len(got), got)
			}
			if got[0].Name != tc.wantFirstNm {
				t.Errorf("首条 name: want %q, got %q", tc.wantFirstNm, got[0].Name)
			}
			if got[0].Value != tc.wantFirstVl {
				t.Errorf("首条 value: want %q, got %q", tc.wantFirstVl, got[0].Value)
			}
			if got[0].Domain != tc.wantFirstDm {
				t.Errorf("首条 domain: want %q, got %q", tc.wantFirstDm, got[0].Domain)
			}
			// header 格式无过期与属性信息：应为会话级、path=/
			if tc.domain != "" && isCookieHeader(tc.raw) {
				if got[0].Path != "/" {
					t.Errorf("header 格式 path 应为 /, got %q", got[0].Path)
				}
				if got[0].Expires != 0 {
					t.Errorf("header 格式 expires 应为 0（会话级）, got %d", got[0].Expires)
				}
			}
		})
	}
}

// TestParseCookieHeader_RealDevToolsSample 用一段贴近真实 DevTools 复制内容的样本，
// 验证长串、含 URL 编码与特殊字符的 cookie 不会被截断或丢弃。
func TestParseCookieHeader_RealDevToolsSample(t *testing.T) {
	raw := "x_host_key_access_https=02bd5330f31b6426e0011b2128f41ce50fdfb042; " +
		"x_zhiyan_staffname=hanezeng; x_zhiyan_project_id=581; " +
		"sensorsdata2015jssdkcross=%7B%22distinct_id%22%3A%22hanezeng%22%2C%22first_id%22%3A%22%22%7D; " +
		"RIO_TOKEN=0.tk0fb4.g-CQh-LOBrdPNZbQQ1"
	got, err := parseCookieHeader(raw, "example.com")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("条数不符: want 5, got %d", len(got))
	}
	byName := map[string]string{}
	for _, c := range got {
		byName[c.Name] = c.Value
	}
	if byName["x_zhiyan_staffname"] != "hanezeng" {
		t.Errorf("x_zhiyan_staffname 解析错误: %q", byName["x_zhiyan_staffname"])
	}
	// URL 编码内容含 %22 等，且值内有 = 号，必须原样保留
	if byName["sensorsdata2015jssdkcross"] != "%7B%22distinct_id%22%3A%22hanezeng%22%2C%22first_id%22%3A%22%22%7D" {
		t.Errorf("URL 编码值被破坏: %q", byName["sensorsdata2015jssdkcross"])
	}
	if byName["RIO_TOKEN"] != "0.tk0fb4.g-CQh-LOBrdPNZbQQ1" {
		t.Errorf("RIO_TOKEN 解析错误: %q", byName["RIO_TOKEN"])
	}
}
