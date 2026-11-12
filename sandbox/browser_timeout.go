package sandbox

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBrowserHandshakeTimeout 是浏览器 MCP initialize 的默认时限。
	// 容器内启动（Xvfb、桌面会话、Node）经常超过通用 MCP 的 8s。
	DefaultBrowserHandshakeTimeout = 3 * time.Minute

	// MinBrowserHandshakeTimeout 拒绝短到必然失败、又会打满重试的配置。
	MinBrowserHandshakeTimeout = time.Second

	// MaxBrowserHandshakeTimeout 给慢启动留余量，同时避免一次握手占住重试循环太久。
	MaxBrowserHandshakeTimeout = 10 * time.Minute
)

// ParseBrowserHandshakeTimeout 解析 sandbox.browser.handshake_timeout。
// 空字符串返回默认值。接受 Go duration（3m、90s）或纯秒数（与 config.Store.GetDuration 一致）。
func ParseBrowserHandshakeTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBrowserHandshakeTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		secs, err2 := strconv.ParseFloat(raw, 64)
		if err2 != nil {
			return 0, fmt.Errorf("sandbox.browser.handshake_timeout: invalid duration %q", raw)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if err := ValidateBrowserHandshakeTimeout(d); err != nil {
		return 0, err
	}
	return d, nil
}

// ValidateBrowserHandshakeTimeout 检查时限是否落在允许范围内。零值和负数不合法。
func ValidateBrowserHandshakeTimeout(d time.Duration) error {
	if d < MinBrowserHandshakeTimeout || d > MaxBrowserHandshakeTimeout {
		return fmt.Errorf("sandbox.browser.handshake_timeout: %s is outside %s-%s",
			d, MinBrowserHandshakeTimeout, MaxBrowserHandshakeTimeout)
	}
	return nil
}
