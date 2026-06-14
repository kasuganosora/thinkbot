package grok

import (
	"net/http"
	"time"

	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// Client
// ============================================================================

// Client 是 xAI (Grok) API 客户端，封装 util/http.Client。
type Client struct {
	http    *httputil.Client
	apiKey  string
	baseURL string
}

// Option 配置 Client。
type Option func(*config)

type config struct {
	apiKey       string
	baseURL      string
	timeout      time.Duration
	maxBodySize  int64
	retryCfg     *retry.Config
	httpClient   *http.Client
	sharedClient *httputil.Client
	dump         bool
}

// WithAPIKey 设置 API Key。
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey = key }
}

// WithBaseURL 设置自定义基础 URL。
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithTimeout 设置 HTTP 超时。
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithMaxBodySize 设置响应体大小上限（字节）。-1 = 无限制。
func WithMaxBodySize(size int64) Option {
	return func(c *config) { c.maxBodySize = size }
}

// WithRetry 设置重试配置。
func WithRetry(cfg retry.Config) Option {
	return func(c *config) { c.retryCfg = &cfg }
}

// WithHTTPClient 使用自定义底层 *http.Client。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.httpClient = hc }
}

// WithSharedClient 使用已有的 *httputil.Client 作为底层 HTTP 客户端。
// 共享其 Transport、连接池、代理等基础设施配置，
// 但各 Provider 会独立设置自己的 baseURL 和认证头，互不影响。
func WithSharedClient(c *httputil.Client) Option {
	return func(cfg *config) { cfg.sharedClient = c }
}

// WithDump 开启 dump 日志。
func WithDump() Option {
	return func(c *config) { c.dump = true }
}

// New 创建一个新的 xAI API Client。
func New(opts ...Option) *Client {
	cfg := &config{
		baseURL: DefaultBaseURL,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	httpOpts := []httputil.Option{
		httputil.WithBaseURL(cfg.baseURL),
		httputil.WithHeader("Authorization", "Bearer "+cfg.apiKey),
	}
	if cfg.timeout > 0 {
		httpOpts = append(httpOpts, httputil.WithTimeout(cfg.timeout))
	}
	if cfg.maxBodySize != 0 {
		httpOpts = append(httpOpts, httputil.WithMaxBodySize(cfg.maxBodySize))
	}
	if cfg.retryCfg != nil {
		httpOpts = append(httpOpts, httputil.WithRetry(*cfg.retryCfg))
	}
	if cfg.httpClient != nil {
		httpOpts = append(httpOpts, httputil.WithHTTPClient(cfg.httpClient))
	}
	if cfg.dump {
		httpOpts = append(httpOpts, httputil.WithDump())
	}

	// 使用共享客户端（Clone 保留 Transport/连接池）或创建新的
	var httpClient *httputil.Client
	if cfg.sharedClient != nil {
		httpClient = cfg.sharedClient.Clone()
	} else {
		httpClient = httputil.New()
	}
	for _, opt := range httpOpts {
		opt(httpClient)
	}

	return &Client{
		http:    httpClient,
		apiKey:  cfg.apiKey,
		baseURL: cfg.baseURL,
	}
}

// ============================================================================
// 内部：请求构建
// ============================================================================

func (c *Client) newRequest(method, path string) *httputil.Request {
	return c.http.NewRequest(method, path)
}
