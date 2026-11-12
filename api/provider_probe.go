package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	probeTimeout = 10 * time.Second
	probeBodyCap = 64 * 1024
	probeMaxHops = 3
)

// ProbeResult 是连通性探测的固定结构。message 不含上游正文和请求头。
type ProbeResult struct {
	OK      bool   `json:"ok"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// probeProvider 用已保存的密钥请求服务商的模型列表。
// 成功只说明列表接口接受了这把密钥，不说明可以生成内容。
func probeProvider(ctx context.Context, def *ProviderDef, client *http.Client) ProbeResult {
	out := ProbeResult{Enabled: def != nil && def.Enabled}
	if def == nil {
		out.Kind = "network"
		out.Message = "provider was unreachable or timed out"
		return out
	}
	if !probeSupportsDiscovery(def.ClientType) {
		out.Kind = "unsupported"
		out.Message = "this client type has no model-list probe; a successful list would not prove generation"
		return out
	}
	if strings.TrimSpace(def.APIKey) == "" {
		out.Kind = "not_configured"
		out.Message = "no API key is stored"
		return out
	}
	base := strings.TrimRight(strings.TrimSpace(def.BaseURL), "/")
	if base == "" {
		out.Kind = "not_configured"
		out.Message = "no base URL is stored"
		return out
	}
	if client == nil {
		client = &http.Client{
			Timeout:   probeTimeout,
			Transport: http.DefaultTransport,
		}
	}
	client = cloneClient(client)
	if client.Timeout <= 0 || client.Timeout > probeTimeout {
		client.Timeout = probeTimeout
	}
	// 调用方自带的 CheckRedirect 不能绕过源检查，否则同主机的 https→http
	// 或换端口的跳转会把 Authorization 带出去。
	prevRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := stripCrossHostAuth(req, via); err != nil {
			return err
		}
		if prevRedirect != nil {
			return prevRedirect(req, via)
		}
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, probeTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		out.Kind = "network"
		out.Message = "provider was unreachable or timed out"
		return out
	}
	req.Header.Set("Authorization", "Bearer "+def.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "different origin") {
			out.Kind = "redirect_blocked"
			out.Message = "refusing to follow a redirect to a different origin"
			return out
		}
		out.Kind = "network"
		out.Message = "provider was unreachable or timed out"
		return out
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyCap))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.OK = true
		out.Kind = "model_list_ok"
		out.Message = "model list reachable with the stored credential; this does not prove generation"
		if !def.Enabled {
			out.Message = "model list reachable, and the provider is disabled so runtime will not use it; this does not prove generation"
		}
	case resp.StatusCode == http.StatusUnauthorized:
		out.Kind = "auth_rejected"
		out.Message = "provider rejected the credentials"
	case resp.StatusCode == http.StatusForbidden:
		out.Kind = "access_denied"
		out.Message = "provider denied access"
	default:
		out.Kind = "upstream_status"
		out.Message = "provider returned an unexpected status"
	}
	return out
}

func probeSupportsDiscovery(clientType string) bool {
	switch strings.ToLower(strings.TrimSpace(clientType)) {
	case "openai compatible", "openai responses", "openai":
		return true
	default:
		return false
	}
}

func cloneClient(c *http.Client) *http.Client {
	next := *c
	return &next
}

func stripCrossHostAuth(req *http.Request, via []*http.Request) error {
	if len(via) >= probeMaxHops {
		return errors.New("stopped after redirects")
	}
	if len(via) == 0 || via[0] == nil || via[0].URL == nil || req == nil || req.URL == nil {
		return errors.New("refusing redirect")
	}
	// 同源才带密钥继续。scheme、主机或端口不同都算另一个源，包括 https 降到 http。
	if requestOrigin(req.URL) != requestOrigin(via[0].URL) {
		return errors.New("refusing credential-bearing redirect to a different origin")
	}
	return nil
}

func requestOrigin(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + host + ":" + port
}
