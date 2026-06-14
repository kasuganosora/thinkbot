package grok

import (
	"context"

	httputil "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// Image Generation
// ============================================================================

// GenerateImage 从文本提示生成图片。
//
// model 通常为 ModelGrokImageQuality。responseFormat 为 ImageFormatURL（默认）或 ImageFormatBase64。
func (c *Client) GenerateImage(ctx context.Context, model, prompt string, opts ...ImageOption) (*ImageResponse, error) {
	req := ImageRequest{
		Model:  model,
		Prompt: prompt,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoGenerateImage(ctx, req)
}

// ImageOption 图片请求选项。
type ImageOption func(*ImageRequest)

// WithImageCount 设置生成数量。
func WithImageCount(n int) ImageOption {
	return func(r *ImageRequest) { r.N = &n }
}

// WithImageFormat 设置响应格式（"url" 或 "b64_json"）。
func WithImageFormat(format string) ImageOption {
	return func(r *ImageRequest) { r.ResponseFormat = format }
}

// WithAspectRatio 设置宽高比（如 "16:9", "1:1"）。
func WithAspectRatio(ratio string) ImageOption {
	return func(r *ImageRequest) { r.AspectRatio = ratio }
}

// WithImageResolution 设置分辨率（"1k" 或 "2k"）。
func WithImageResolution(res string) ImageOption {
	return func(r *ImageRequest) { r.Resolution = res }
}

// DoGenerateImage 发送完整的图片生成请求。
func (c *Client) DoGenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	resp, err := c.newRequest("POST", "/v1/images/generations").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ImageResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Image Editing
// ============================================================================

// EditImage 编辑现有图片。
//
// imageURL 可以是公开 URL 或 base64 data URI（如 "data:image/png;base64,..."）。
func (c *Client) EditImage(ctx context.Context, model, prompt, imageURL string, opts ...ImageOption) (*ImageResponse, error) {
	req := ImageRequest{
		Model:    model,
		Prompt:   prompt,
		ImageURL: imageURL,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoEditImage(ctx, req)
}

// DoEditImage 发送完整的图片编辑请求。
func (c *Client) DoEditImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	if req.ImageURL == "" {
		return nil, grokError("grok: image URL is required for image editing")
	}
	resp, err := c.newRequest("POST", "/v1/images/edits").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ImageResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// 辅助函数
// ============================================================================

// grokError 创建一个简单的 error。
func grokError(msg string) error {
	return &APIError{Message: msg}
}

// FirstImageURL 返回第一个图片的 URL，如果没有则返回空字符串。
func (r *ImageResponse) FirstImageURL() string {
	if len(r.Data) > 0 {
		return r.Data[0].URL
	}
	return ""
}

// FirstImageBase64 返回第一个图片的 base64 数据，如果没有则返回空字符串。
func (r *ImageResponse) FirstImageBase64() string {
	if len(r.Data) > 0 {
		return r.Data[0].B64JSON
	}
	return ""
}

// 确保引用 httputil（供 dump 等功能在将来扩展时使用）。
var _ = httputil.DefaultClient
