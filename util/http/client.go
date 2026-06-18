package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// Client
// ============================================================================

// 默认响应体大小上限。
const defaultMaxBodySize = 10 * 1024 * 1024 // 10MB

// Client 封装了 net/http.Client，集成了日志、重试、看门狗支持。
type Client struct {
	httpClient  *http.Client
	baseURL     string
	headers     map[string]string
	retryCfg    *retry.Config
	maxBodySize int64 // 响应体最大字节数，0 = 使用 defaultMaxBodySize
	dumpEnabled bool  // 是否默认开启 dump
}

// Option 配置 Client。
type Option func(*Client)

// WithBaseURL 设置基础 URL，后续请求的相对路径会拼接在其后。
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
}

// WithTimeout 设置 HTTP 客户端超时。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.httpClient.Timeout = d
	}
}

// WithHeader 添加默认请求头（每次请求都会携带）。
func WithHeader(key, value string) Option {
	return func(c *Client) {
		c.headers[key] = value
	}
}

// WithHeaders 批量设置默认请求头。
func WithHeaders(headers map[string]string) Option {
	return func(c *Client) {
		for k, v := range headers {
			c.headers[k] = v
		}
	}
}

// WithHTTPClient 使用自定义的 *http.Client。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithRetry 设置重试配置。
func WithRetry(cfg retry.Config) Option {
	return func(c *Client) {
		c.retryCfg = &cfg
	}
}

// WithRetrySimple 快捷设置简单重试（固定次数 + 固定间隔）。
func WithRetrySimple(maxRetries int, interval time.Duration) Option {
	return func(c *Client) {
		c.retryCfg = &retry.Config{
			MaxRetries:    maxRetries,
			FixedInterval: interval,
		}
	}
}

// WithMaxBodySize 设置响应体最大大小（字节），防止 OOM。
// 默认 10MB。设为 -1 表示无限制。
func WithMaxBodySize(size int64) Option {
	return func(c *Client) {
		c.maxBodySize = size
	}
}

// WithDump 默认开启所有请求/响应的 dump 日志。
func WithDump() Option {
	return func(c *Client) {
		c.dumpEnabled = true
	}
}

// WithProxy 设置 HTTP/HTTPS/SOCKS5 代理。
//
// 支持的 URL 格式：
//   - http://host:port
//   - https://host:port
//   - socks5://host:port
//   - socks5h://host:port（DNS 也走代理）
//
// 如果 proxyURL 为空则不做任何操作。如果 proxyURL 解析失败会记录警告日志并忽略。
func WithProxy(proxyURL string) Option {
	return func(c *Client) {
		if proxyURL == "" {
			return
		}
		u, err := url.Parse(proxyURL)
		if err != nil {
			log.Logger.Warnw("invalid proxy URL, ignoring", "proxy", proxyURL, "err", err)
			return
		}
		c.ensureTransport().Proxy = http.ProxyURL(u)
	}
}

// WithProxyFromEnv 从环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）读取代理配置。
func WithProxyFromEnv() Option {
	return func(c *Client) {
		c.ensureTransport().Proxy = http.ProxyFromEnvironment
	}
}

// New 创建一个新的 Client。
func New(opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		headers: make(map[string]string),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DefaultClient 返回一个使用默认配置的 Client。
func DefaultClient() *Client {
	return New()
}

// Clone 创建 Client 的浅拷贝。
//
// 底层的 *http.Client（含 Transport、连接池、TLS 配置）被共享，
// 但 headers、baseURL、retryCfg 等字段拥有独立的副本，可安全修改而不影响原 Client。
// 适用于多个 Provider 共享同一 HTTP Transport 的场景。
func (c *Client) Clone() *Client {
	clone := &Client{
		httpClient:  c.httpClient, // 共享 Transport / 连接池
		baseURL:     c.baseURL,
		headers:     make(map[string]string, len(c.headers)),
		retryCfg:    c.retryCfg,
		maxBodySize: c.maxBodySize,
		dumpEnabled: c.dumpEnabled,
	}
	for k, v := range c.headers {
		clone.headers[k] = v
	}
	return clone
}

// ensureTransport 确保 httpClient 有一个 *http.Transport，如果没有则创建。
// 如果 Transport 已被设置为非 *http.Transport 类型，则替换为新的 Transport。
func (c *Client) ensureTransport() *http.Transport {
	if t, ok := c.httpClient.Transport.(*http.Transport); ok {
		return t
	}
	t := &http.Transport{}
	c.httpClient.Transport = t
	return t
}

// ============================================================================
// Request 构造器
// ============================================================================

// Request 表示一个 HTTP 请求，使用链式 API 构造。
type Request struct {
	client    *Client
	method    string
	url       string
	headers   map[string]string
	query     url.Values
	body      any
	ctx       context.Context
	retryCfg    *retry.Config // per-request 重试覆盖（可选）
	dump        bool          // 是否 dump 本请求的详细信息
	sseLastEventID string     // SSE 最后收到的事件 ID（用于自动重连）
}

// NewRequest 创建一个新请求构造器。
func (c *Client) NewRequest(method, path string) *Request {
	fullURL := path
	if c.baseURL != "" && !strings.HasPrefix(strings.ToLower(path), "http://") && !strings.HasPrefix(strings.ToLower(path), "https://") {
		fullURL = c.baseURL + "/" + strings.TrimLeft(path, "/")
	}
	r := &Request{
		client:  c,
		method:  method,
		url:     fullURL,
		headers: make(map[string]string),
		ctx:     context.Background(),
		dump:    c.dumpEnabled,
	}
	// 继承客户端默认 headers
	for k, v := range c.headers {
		r.headers[k] = v
	}
	return r
}

// Get 快捷创建 GET 请求。
func (c *Client) Get(path string) *Request { return c.NewRequest(http.MethodGet, path) }

// Post 快捷创建 POST 请求。
func (c *Client) Post(path string) *Request { return c.NewRequest(http.MethodPost, path) }

// Put 快捷创建 PUT 请求。
func (c *Client) Put(path string) *Request { return c.NewRequest(http.MethodPut, path) }

// Patch 快捷创建 PATCH 请求。
func (c *Client) Patch(path string) *Request { return c.NewRequest(http.MethodPatch, path) }

// Delete 快捷创建 DELETE 请求。
func (c *Client) Delete(path string) *Request { return c.NewRequest(http.MethodDelete, path) }

// SetHeader 设置请求头。
func (r *Request) SetHeader(key, value string) *Request {
	r.headers[key] = value
	return r
}

// SetHeaders 批量设置请求头。
func (r *Request) SetHeaders(headers map[string]string) *Request {
	for k, v := range headers {
		r.headers[k] = v
	}
	return r
}

// SetQuery 设置查询参数。
func (r *Request) SetQuery(key, value string) *Request {
	if r.query == nil {
		r.query = make(url.Values)
	}
	r.query.Set(key, value)
	return r
}

// SetQueryValues 批量设置查询参数。
func (r *Request) SetQueryValues(values url.Values) *Request {
	if r.query == nil {
		r.query = make(url.Values)
	}
	for k, vs := range values {
		for _, v := range vs {
			r.query.Add(k, v)
		}
	}
	return r
}

// SetJSONBody 设置 JSON 请求体，并自动添加 Content-Type: application/json。
func (r *Request) SetJSONBody(body any) *Request {
	r.body = body
	r.headers["Content-Type"] = "application/json"
	return r
}

// SetBody 设置原始请求体。
func (r *Request) SetBody(body io.Reader) *Request {
	r.body = body
	return r
}

// SetMultipart 设置 multipart/form-data 请求体。
// 会自动关闭表单写入器并设置正确的 Content-Type（含 boundary）。
func (r *Request) SetMultipart(form *MultipartForm) *Request {
	form.close()
	r.body = form.bytes()
	r.headers["Content-Type"] = form.ContentType()
	return r
}

// SetContext 设置 context。
func (r *Request) SetContext(ctx context.Context) *Request {
	if ctx != nil {
		r.ctx = ctx
	}
	return r
}

// SetRetry 覆盖此请求的重试配置（不影响 Client 级别配置）。
func (r *Request) SetRetry(cfg retry.Config) *Request {
	r.retryCfg = &cfg
	return r
}

// Dump 开启此请求的 dump 日志（打印完整请求和响应信息）。
func (r *Request) Dump() *Request {
	r.dump = true
	return r
}

// BearerToken 快捷设置 Bearer 认证头。
func (r *Request) BearerToken(token string) *Request {
	r.headers["Authorization"] = "Bearer " + token
	return r
}

// BasicAuth 快捷设置 Basic 认证。
func (r *Request) BasicAuth(username, password string) *Request {
	r.SetHeader("Authorization", "Basic "+basicAuth(username, password))
	return r
}

// ============================================================================
// 执行
// ============================================================================

// Response 表示 HTTP 响应。
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// JSON 将响应体反序列化到 v。
func (r *Response) JSON(v any) error {
	if len(r.Body) == 0 {
		return nil
	}
	return json.Unmarshal(r.Body, v)
}

// String 返回响应体的字符串形式。
func (r *Response) String() string {
	return string(r.Body)
}

// IsSuccess 判断响应是否为 2xx。
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// Do 执行请求，返回 Response。
//
// 如果配置了重试，会自动对可重试的状态码（5xx、429）和网络错误进行重试。
// 响应体会被完整读入内存。
func (r *Request) Do() (*Response, error) {
	// per-request retry 优先于 client 级别
	if r.retryCfg != nil {
		return r.doWithRetry()
	}
	// --- 有重试配置 ---
	if r.client.retryCfg != nil {
		return r.doWithRetry()
	}
	// --- 无重试 ---
	return r.doOnce()
}

// doOnce 单次执行请求。
func (r *Request) doOnce() (*Response, error) {
	req, err := r.buildHTTPRequest()
	if err != nil {
		return nil, errs.Wrap(err, "failed to build HTTP request")
	}

	// dump 请求
	if r.dump {
		r.dumpRequest(req)
	}

	start := time.Now()
	resp, err := r.client.httpClient.Do(req)
	if err != nil {
		log.Logger.Warnw("http request failed",
			"method", r.method, "url", req.URL.String(), "err", err, "elapsed", time.Since(start))
		return nil, errs.Wrap(err, "http request failed")
	}
	defer resp.Body.Close()

	// 响应体大小限制
	bodyReader := resp.Body
	maxSize := r.client.maxBodySize
	if maxSize == 0 {
		maxSize = defaultMaxBodySize
	}
	if maxSize > 0 {
		bodyReader = io.NopCloser(io.LimitReader(resp.Body, maxSize+1))
	}

	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, errs.Wrap(err, "failed to read response body")
	}
	if maxSize > 0 && int64(len(body)) > maxSize {
		log.Logger.Warnw("http response body truncated",
			"url", req.URL.String(), "max_size", maxSize, "actual_size", len(body))
		body = body[:maxSize]
	}

	result := &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}

	log.Logger.Debugw("http response",
		"method", r.method, "url", req.URL.String(),
		"status", resp.StatusCode, "body_len", len(body), "elapsed", time.Since(start))

	// dump 响应
	if r.dump {
		r.dumpResponse(result, time.Since(start))
	}

	if !result.IsSuccess() {
		return result, errs.HTTPErrorf(resp.StatusCode,
			"http %s %s -> %d: %s", r.method, req.URL.String(), resp.StatusCode, truncate(string(body), 500))
	}

	return result, nil
}

// doWithRetry 带重试执行请求。
func (r *Request) doWithRetry() (*Response, error) {
	// per-request retry 优先
	cfgPtr := r.retryCfg
	if cfgPtr == nil {
		cfgPtr = r.client.retryCfg
	}
	cfg := *cfgPtr // copy
	name := fmt.Sprintf("%s %s", r.method, r.url)

	// 默认 ShouldRetry：仅对可重试 HTTP 状态码（5xx、429、网络错误）重试
	if cfg.ShouldRetry == nil {
		cfg.ShouldRetry = func(attempt int, err error) bool {
			return isRetryableCode(errs.GetCode(err))
		}
	}

	// 捕获最近一次 429 响应的 Retry-After，用于覆盖退避时间
	var lastRetryAfter time.Duration
	userGetRetryDelay := cfg.GetRetryDelay // 保存用户已有的回调（可能为 nil）
	cfg.GetRetryDelay = func(err error) time.Duration {
		// 优先使用用户自定义的 GetRetryDelay
		if userGetRetryDelay != nil {
			if d := userGetRetryDelay(err); d > lastRetryAfter {
				return d
			}
		}
		return lastRetryAfter
	}

	var result *Response
	res := retry.Do(r.ctx, name, cfg, func(ctx context.Context) error {
		r.ctx = ctx
		// 重置上次记录的 Retry-After
		lastRetryAfter = 0
		resp, err := r.doOnce()
		if err != nil {
			// 从 429 响应中提取 Retry-After
			if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = parseRetryAfter(resp.Headers.Get("Retry-After"))
			}
			return err
		}
		result = resp
		return nil
	})

	if res.Err != nil {
		return result, res.Err
	}
	return result, nil
}

// buildHTTPRequest 从 Request 构建 *http.Request。
func (r *Request) buildHTTPRequest() (*http.Request, error) {
	// --- 构建完整 URL ---
	fullURL := r.url
	if len(r.query) > 0 {
		fullURL += "?" + r.query.Encode()
	}

	// --- 构建请求体 ---
	var bodyReader io.Reader
	if r.body != nil {
		switch v := r.body.(type) {
		case io.Reader:
			bodyReader = v
		case []byte:
			bodyReader = bytes.NewReader(v)
		case string:
			bodyReader = strings.NewReader(v)
		default:
			// JSON 序列化
			data, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("marshal json body: %w", err)
			}
			bodyReader = bytes.NewReader(data)
		}
	}

	req, err := http.NewRequestWithContext(r.ctx, r.method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// --- 请求头 ---
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

// ============================================================================
// 便捷方法
// ============================================================================

// GetJSON 发送 GET 请求并将 JSON 响应解析到 v。
func (c *Client) GetJSON(ctx context.Context, path string, v any, headers ...map[string]string) error {
	req := c.Get(path).SetContext(ctx)
	for _, h := range headers {
		req.SetHeaders(h)
	}
	resp, err := req.Do()
	if err != nil {
		return err
	}
	return resp.JSON(v)
}

// PostJSON 发送 POST 请求（JSON body）并将 JSON 响应解析到 v。
func (c *Client) PostJSON(ctx context.Context, path string, body, v any, headers ...map[string]string) error {
	req := c.Post(path).SetContext(ctx).SetJSONBody(body)
	for _, h := range headers {
		req.SetHeaders(h)
	}
	resp, err := req.Do()
	if err != nil {
		return err
	}
	if v != nil {
		return resp.JSON(v)
	}
	return nil
}

// ============================================================================
// 内部工具
// ============================================================================

// isRetryableCode 判断 HTTP 状态码是否值得重试。
func isRetryableCode(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusBadGateway,           // 502
		http.StatusServiceUnavailable,   // 503
		http.StatusGatewayTimeout:       // 504
		return true
	case 0: // 网络错误，没有状态码
		return true
	default:
		return code >= 500
	}
}

// parseRetryAfter 解析 HTTP Retry-After 响应头。
// 支持两种格式：
//   - 秒数（如 "120"）
//   - HTTP-date（如 "Wed, 21 Oct 2025 07:28:00 GMT"）
//
// 返回 0 表示无法解析或值无效。
func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 0
	}
	// 尝试解析为秒数
	if seconds, err := strconv.Atoi(val); err == nil {
		if seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	// 尝试解析为 HTTP-date
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// basicAuth 生成 Basic 认证字符串（base64 编码）。
func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// truncate 截断字符串到 maxLen 个 rune（避免截断多字节 UTF-8 字符中间）。
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// ============================================================================
// Dump
// ============================================================================

// dumpRequest 将完整请求信息打印到日志。
func (r *Request) dumpRequest(req *http.Request) {
	var b strings.Builder
	b.WriteString("\n========== HTTP Request Dump ==========\n")
	b.WriteString(fmt.Sprintf("%s %s\n", req.Method, req.URL.String()))
	b.WriteString("--- Headers ---\n")
	for k, vs := range req.Header {
		for _, v := range vs {
			// 脱敏 Authorization
			if strings.EqualFold(k, "Authorization") {
				v = "***"
			}
			b.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
	}
	reqContentType := req.Header.Get("Content-Type")
	textReq := isTextContentType(reqContentType)
	if r.body != nil {
		switch v := r.body.(type) {
		case string:
			if textReq {
				b.WriteString("--- Body ---\n")
				b.WriteString(v)
				b.WriteString("\n")
			} else {
				b.WriteString(fmt.Sprintf("--- Body (binary, content-length=%d) ---\n", len(v)))
			}
		case []byte:
			if textReq {
				b.WriteString("--- Body (bytes) ---\n")
				b.WriteString(string(v))
				b.WriteString("\n")
			} else {
				b.WriteString(fmt.Sprintf("--- Body (binary, content-length=%d) ---\n", len(v)))
			}
		case io.Reader:
			b.WriteString("--- Body (io.Reader, not dumped) ---\n")
		default:
			// JSON body — 直接序列化（一定是文本类型）
			data, err := json.MarshalIndent(v, "", "  ")
			if err == nil {
				b.WriteString("--- Body (JSON) ---\n")
				b.WriteString(string(data))
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("========================================")
	log.Logger.Infow("http request dump", "detail", b.String())
}

// dumpResponse 将完整响应信息打印到日志。
// 对于文本类响应（text/*、application/json、application/xml、application/javascript、
// text/event-stream 等）会打印完整响应体；
// 对于二进制类响应（图片、音视频、application/octet-stream 等）仅显示 Content-Length。
func (r *Request) dumpResponse(resp *Response, elapsed time.Duration) {
	var b strings.Builder
	b.WriteString("\n========== HTTP Response Dump ==========\n")
	b.WriteString(fmt.Sprintf("%s %s -> %d (%v)\n", r.method, r.url, resp.StatusCode, elapsed))
	b.WriteString("--- Headers ---\n")
	for k, vs := range resp.Headers {
		for _, v := range vs {
			b.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
	}

	contentType := resp.Headers.Get("Content-Type")
	if isTextContentType(contentType) {
		b.WriteString("--- Body ---\n")
		b.WriteString(string(resp.Body))
		b.WriteString("\n")
	} else {
		b.WriteString(fmt.Sprintf("--- Body (binary, not dumped, content-length=%d) ---\n", len(resp.Body)))
	}
	b.WriteString("========================================")
	log.Logger.Infow("http response dump", "detail", b.String())
}

// isTextContentType 判断 Content-Type 是否为可安全 dump 的文本类型。
func isTextContentType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		return true // 没有 Content-Type 时默认可 dump
	}
	// SSE
	if ct == "text/event-stream" {
		return true
	}
	// text/*
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	// 常见 application 文本类型
	switch ct {
	case "application/json",
		"application/xml",
		"application/javascript",
		"application/x-www-form-urlencoded",
		"application/xhtml+xml",
		"application/ld+json",
		"application/manifest+json",
		"application/graphql",
		"application/soap+xml":
		return true
	}
	// application/*+json / application/*+xml
	if strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml") {
		return true
	}
	return false
}
