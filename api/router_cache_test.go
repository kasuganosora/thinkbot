package api

import "testing"

// TestHashedAssetRE_MatchesViteOutput 验证带内容哈希的构建产物被识别为可长缓存。
func TestHashedAssetRE_MatchesViteOutput(t *testing.T) {
	hashed := []string{
		"/assets/index-BymryakM.js",
		"/assets/Chat-B3U2oJJs.js",
		"/assets/XtermConsole-Ditn7iiM.js",
		"/assets/style-a1b2c3d4.css",
		"/assets/index-BymryakM.js.map",
		"/assets/logo-1a2b3c4d5e.svg",
		"/assets/font-AbCdEf12.woff2",
	}
	for _, p := range hashed {
		if !hashedAssetRE.MatchString(p) {
			t.Errorf("hashedAssetRE should match hashed asset %q", p)
		}
	}
}

// TestHashedAssetRE_RejectsUnhashed 验证入口文档与无哈希资源不会被长缓存。
//
// 这是本次事故的核心：index.html 一旦被缓存，前端发版后用户刷新仍加载旧 chunk，
// 表现为「代码改了但页面没变」。
func TestHashedAssetRE_RejectsUnhashed(t *testing.T) {
	unhashed := []string{
		"/",
		"/index.html",
		"/favicon.ico",
		"/manifest.json",
		"/assets/style.css", // 无哈希
		"/robots.txt",
		"/logo.png",
		// 哈希段过短，不足以保证唯一，按保守策略不长缓存
		"/assets/a-1b2c.js",
	}
	for _, p := range unhashed {
		if hashedAssetRE.MatchString(p) {
			t.Errorf("hashedAssetRE must NOT match %q (would serve stale content after a release)", p)
		}
	}
}
