package notify

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseCIDRs 解析逗号分隔的网段列表；裸 IP 视为 /32 或 /128。非法项跳过并返回。
func ParseCIDRs(s string) (prefixes []netip.Prefix, invalid []string) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				invalid = append(invalid, part)
				continue
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			invalid = append(invalid, part)
			continue
		}
		prefixes = append(prefixes, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return prefixes, invalid
}

// ContainsIP 判断 ip 是否落在任一网段内（IPv4-mapped IPv6 先解映射）。
func ContainsIP(prefixes []netip.Prefix, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP 确定请求的真实来源 IP。
//
// 与 gin 默认的 ClientIP（信任任何人的 X-Forwarded-For）不同：仅当直连对端
// （RemoteAddr）落在 trusted 网段时才采信转发头，并从 X-Forwarded-For 右侧起
// 跳过可信代理取第一个不可信地址；XFF 缺失时退回 X-Real-IP。这样公网请求经
// nginx 转发后仍以公网 IP 参与 CIDR 判定，伪造的 XFF 左侧项无效。
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := remoteAddr(r)
	if !peer.IsValid() || !ContainsIP(trusted, peer) {
		return peer
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		var hops []string
		for _, h := range xff {
			for _, p := range strings.Split(h, ",") {
				if p = strings.TrimSpace(p); p != "" {
					hops = append(hops, p)
				}
			}
		}
		for i := len(hops) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(stripPort(hops[i]))
			if err != nil {
				// 无法解析的转发项：保守视为不可信来源（拒绝放行）。
				return netip.Addr{}
			}
			a = a.Unmap()
			if !ContainsIP(trusted, a) {
				return a
			}
			if i == 0 {
				return a
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		a, err := netip.ParseAddr(stripPort(xr))
		if err != nil {
			return netip.Addr{}
		}
		return a.Unmap()
	}
	return peer
}

func remoteAddr(r *http.Request) netip.Addr {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func stripPort(s string) string {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return strings.Trim(s, "[]")
}
