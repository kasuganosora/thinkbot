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

// 企业微信智能机器人的媒体文件**不在** qq.com / weixin.qq.com 域上：回调里的下载
// URL 指向腾讯云 COS 的官方桶（现网实测：
// ww-aibot-img-1258476243.cos.ap-guangzhou.myqcloud.com，签名 5 分钟有效）。
//
// ⚠️ 2026-10-27 线上事故：只按域名后缀放行时这个 host 被拒，当天 image 与 file
// 全部下载失败，用户侧只看到「[图片：下载或解密失败]」。
//
// 但不能直接放行 "myqcloud.com"——那是**所有**腾讯云租户的公有桶，等于允许
// 伪造的回调 URL 让 bot 去拉任意公开对象并喂给 LLM。故用「官方桶名前缀 +
// 域名后缀」双重限定，既能覆盖未来的 region/appid 变体，又不放开整个 COS。
const (
	officialMediaHostPrefix = "ww-aibot-"
	officialMediaHostSuffix = ".myqcloud.com"
)

// isOfficialMediaHost 判断是否企业微信智能机器人媒体的官方 COS 桶域名。
func isOfficialMediaHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if !strings.HasPrefix(h, officialMediaHostPrefix) || !strings.HasSuffix(h, officialMediaHostSuffix) {
		return false
	}
	// 前缀与后缀之间必须还有内容："ww-aibot-.myqcloud.com" 不放行。
	return len(h) > len(officialMediaHostPrefix)+len(officialMediaHostSuffix)
}

// officialEndpointHosts 是官方长连接主机。
var officialEndpointHosts = []string{"openws.work.weixin.qq.com"}

// hostMatchesSuffix 判断 host 是否为 suffix 本身或其子域（"evilqq.com" 不匹配 "qq.com"）。
func hostMatchesSuffix(host, suffix string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	suffix = strings.ToLower(suffix)
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// isAllowedMediaHost 判断媒体主机是否放行。
//
// extra 是运维额外放行的主机后缀（Config.MediaAllowedHosts）：企微若哪天换了桶名，
// 线上不必等发版就能自救。缺省为空，官方桶与后缀白名单照旧生效。
func isAllowedMediaHost(host string, extra []string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false // 字面量 IP 一律拒绝
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range allowedMediaSuffixes {
		if hostMatchesSuffix(h, s) {
			return true
		}
	}
	if isOfficialMediaHost(h) {
		return true
	}
	for _, s := range extra {
		if s != "" && hostMatchesSuffix(h, s) {
			return true
		}
	}
	return false
}

// validateMediaURL 校验入站媒体 URL：必须 https 且主机在白名单内。
func validateMediaURL(raw string, extra []string) error {
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
	if !isAllowedMediaHost(u.Hostname(), extra) {
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
func newMediaTransportClient(extra []string) *nethttp.Client {
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
			return validateMediaURL(req.URL.String(), extra)
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
