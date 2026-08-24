// Package buildinfo 暴露当前运行二进制的构建信息（git 版本、构建时间）。
//
// 用途：健康探针回报这些信息，可在不登录机器、不 dump 二进制的情况下，
// 一眼确认「运行中的实例到底是不是包含某次修复的那版」——这正是排查
// 「改了代码但现象没变」类问题时的关键观测点。
//
// 信息来源（按优先级）：
//  1. 构建时通过 -ldflags "-X ..." 注入（最准，部署脚本应固化此步）；
//  2. 未注入时退回二进制文件 mtime（仍是真实编译时间，多数情况可用）；
//  3. git revision 仍未取到时，尝试 `git rev-parse HEAD`（仅仓库内运行时有意义）。
package buildinfo

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 以下变量通过构建时 -ldflags 注入。示例：
//
//	go build \
//	  -ldflags "-s -w \
//	    -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=$(git rev-parse HEAD) \
//	    -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
//	    -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
//	  -o thinkbot ./cmd
var (
	GitRevision = "unknown" // 完整 commit hash
	BuildTime   = "unknown" // RFC3339 构建时间，例 2026-08-24T09:00:00Z
	Version     = "dev"     // 语义版本 / tag
)

var (
	once     sync.Once
	resolved resolvedInfo
)

type resolvedInfo struct {
	gitShort  string
	buildTime time.Time
	source    string // ldflags | binary-mtime | unknown
}

func resolve() {
	once.Do(func() {
		short := GitRevision
		if len(short) > 12 {
			short = short[:12]
		}
		resolved.gitShort = short

		// 优先：构建时注入的 BuildTime
		if BuildTime != "" && BuildTime != "unknown" {
			if t, err := time.Parse(time.RFC3339, BuildTime); err == nil {
				resolved.buildTime = t
				resolved.source = "ldflags"
				return
			}
		}

		// 兜底 1：二进制文件 mtime 即编译时间（mv 替换会保留源文件 mtime）
		if exe, err := os.Executable(); err == nil {
			if fi, err := os.Stat(exe); err == nil {
				resolved.buildTime = fi.ModTime()
				resolved.source = "binary-mtime"
				return
			}
		}

		// 兜底 2：revision 仍未取到，尝试 git（仓库内运行时）
		if GitRevision == "" || GitRevision == "unknown" {
			if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
				if h := strings.TrimSpace(string(out)); h != "" {
					GitRevision = h
					if len(h) > 12 {
						resolved.gitShort = h[:12]
					} else {
						resolved.gitShort = h
					}
				}
			}
		}
		resolved.source = "unknown"
	})
}

// Info 当前运行二进制的构建信息快照。
type Info struct {
	Version       string `json:"version"`
	GitRevision   string `json:"gitRevision"`
	GitShort      string `json:"gitShort"`
	BuildTime     string `json:"buildTime"`     // RFC3339；未知时为空串
	BuildTimeUnix int64  `json:"buildTimeUnix"` // unix 秒；未知时为 0
	Source        string `json:"source"`        // 信息来源：ldflags / binary-mtime / unknown
	GoVersion     string `json:"goVersion"`
}

// Get 返回构建信息。解析与缓存只在首次调用时发生。
func Get() Info {
	resolve()
	rt := resolved.buildTime
	var rtUnix int64
	var bt string
	if !rt.IsZero() {
		rtUnix = rt.Unix()
		bt = rt.Format(time.RFC3339)
	}
	return Info{
		Version:       Version,
		GitRevision:   GitRevision,
		GitShort:      resolved.gitShort,
		BuildTime:     bt,
		BuildTimeUnix: rtUnix,
		Source:        resolved.source,
		GoVersion:     runtime.Version(),
	}
}
