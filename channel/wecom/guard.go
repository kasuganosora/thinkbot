package wecom

import (
	"errors"
	"fmt"
	"net"
	nethttp "net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// 出站目标白名单（媒体下载与长连接地址）。
//
// 媒体 URL 来自回调帧，原样 GET 会形成 SSRF 面：这里要求 https + 官方域名后缀，
// 并在**拨号时**（DNS 解析之后）拒绝私网/回环/链路本地等地址，防 DNS rebinding。
// 重定向目标同样逐跳校验。

// allowedMediaSuffixes 是允许的媒体主机后缀（含自身与任意子域，精确后缀匹配）。
var allowedMediaSuffixes = []string{"qq.com", "weixin.qq.com"}

// officialEndpointHosts 是官方长连接主机。
var officialEndpointHosts = []string{"openws.work.weixin.qq.com"}

// hostMatchesSuffix 判断 host 是否为 suffix 本身或其子域（"evilqq.com" 不匹配 "qq.com"）。
func hostMatchesSuffix(host, suffix string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	suffix = strings.ToLower(suffix)
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

func isAllowedMediaHost(host string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false // 字面量 IP 一律拒绝
	}
	for _, s := range allowedMediaSuffixes {
		if hostMatchesSuffix(host, s) {
			return true
		}
	}
	return false
}

// validateMediaURL 校验入站媒体 URL：必须 https 且主机在白名单内。
func validateMediaURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("wecom: invalid media url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("wecom: media url scheme %q not allowed (https only)", u.Scheme)
	}
	if u.User != nil {
		return errors.New("wecom: media url must not carry userinfo")
	}
	if !isAllowedMediaHost(u.Hostname()) {
		return fmt.Errorf("wecom: media host %q not in allowlist", u.Hostname())
	}
	return nil
}

// isBlockedIP 判断解析后的地址是否属于不允许访问的网段。
func isBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() ||
		// 100.64.0.0/10 运营商级 NAT，常被用作内网
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// dialControl 在 connect 之前检查实际要连的 IP（DNS 解析之后）。
func dialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("wecom: unexpected dial address %q", address)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("wecom: refusing to dial non-public address %s", ip)
	}
	return nil
}

// newMediaTransportClient 构造媒体下载用的 *http.Client：拨号层 IP 校验 + 重定向白名单。
func newMediaTransportClient() *nethttp.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl}
	tr := &nethttp.Transport{
		Proxy:                 nil, // 不走环境代理：代理会绕过拨号层的 IP 校验
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: wecomMediaTimeout,
	}
	return &nethttp.Client{
		Transport: tr,
		CheckRedirect: func(req *nethttp.Request, via []*nethttp.Request) error {
			if len(via) >= 5 {
				return errors.New("wecom: too many redirects")
			}
			return validateMediaURL(req.URL.String())
		},
	}
}

// validateEndpoint 校验长连接地址：必须 wss:// + 官方主机；allowCustom 时只要求 wss://。
// 返回 (是否使用了自定义地址, error)。
func validateEndpoint(raw string, allowCustom bool) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, fmt.Errorf("wecom: invalid endpoint: %w", err)
	}
	if u.Scheme != "wss" {
		return false, fmt.Errorf("wecom: endpoint scheme %q not allowed (wss only)", u.Scheme)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	for _, h := range officialEndpointHosts {
		if host == h {
			return false, nil
		}
	}
	if !allowCustom {
		return false, fmt.Errorf("wecom: endpoint host %q is not official; set allowCustomEndpoint to opt in", host)
	}
	return true, nil
}
