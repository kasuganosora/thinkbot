package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ============================================================================
// Video Generation — 自动轮询
// ============================================================================

// VideoOption 视频请求选项。
type VideoOption func(*VideoGenerationRequest)

// WithVideoDuration 设置视频时长（秒，1-15）。
func WithVideoDuration(d int) VideoOption {
	return func(r *VideoGenerationRequest) { r.Duration = &d }
}

// WithVideoAspectRatio 设置宽高比。
func WithVideoAspectRatio(ratio string) VideoOption {
	return func(r *VideoGenerationRequest) { r.AspectRatio = ratio }
}

// WithVideoResolution 设置分辨率（"480p" 或 "720p"）。
func WithVideoResolution(res string) VideoOption {
	return func(r *VideoGenerationRequest) { r.Resolution = res }
}

// WithVideoImage 设置输入图片（image-to-video 模式）。
func WithVideoImage(imageURL string) VideoOption {
	return func(r *VideoGenerationRequest) { r.Image = &VideoImage{URL: imageURL} }
}

// GenerateVideo 生成视频，自动轮询直到完成或超时。
//
// 默认超时 10 分钟，轮询间隔 5 秒。
func (c *Client) GenerateVideo(ctx context.Context, model, prompt string, opts ...VideoOption) (*VideoResult, error) {
	return c.GenerateVideoWithPolling(ctx, model, prompt, 10*time.Minute, 5*time.Second, opts...)
}

// GenerateVideoWithPolling 生成视频，自定义超时和轮询间隔。
func (c *Client) GenerateVideoWithPolling(
	ctx context.Context,
	model, prompt string,
	timeout, interval time.Duration,
	opts ...VideoOption,
) (*VideoResult, error) {
	req := VideoGenerationRequest{
		Model:  model,
		Prompt: prompt,
	}
	for _, opt := range opts {
		opt(&req)
	}

	start, err := c.StartVideoGeneration(ctx, req)
	if err != nil {
		return nil, err
	}

	return c.PollVideo(ctx, start.RequestID, timeout, interval)
}

// ============================================================================
// 手动两步流程
// ============================================================================

// StartVideoGeneration 提交视频生成请求，返回 request_id。
func (c *Client) StartVideoGeneration(ctx context.Context, req VideoGenerationRequest) (*VideoStartResponse, error) {
	if req.Model == "" {
		return nil, errors.New("grok: model is required")
	}
	if req.Prompt == "" {
		return nil, errors.New("grok: prompt is required")
	}

	resp, err := c.newRequest("POST", "/v1/videos/generations").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result VideoStartResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetVideoStatus 查询视频生成状态。
func (c *Client) GetVideoStatus(ctx context.Context, requestID string) (*VideoStatusResponse, error) {
	resp, err := c.newRequest("GET", "/v1/videos/"+requestID).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result VideoStatusResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// PollVideo 轮询视频生成状态直到完成、失败或超时。
func (c *Client) PollVideo(ctx context.Context, requestID string, timeout, interval time.Duration) (*VideoResult, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("grok: video generation timed out after %v", timeout)
			}

			status, err := c.GetVideoStatus(ctx, requestID)
			if err != nil {
				return nil, err
			}

			switch status.Status {
			case VideoStatusDone:
				if status.Video == nil {
					return nil, errors.New("grok: video generation completed but no video URL returned")
				}
				return status.Video, nil
			case VideoStatusFailed:
				if status.Error != nil {
					return nil, fmt.Errorf("grok: video generation failed [%s]: %s", status.Error.Code, status.Error.Message)
				}
				return nil, errors.New("grok: video generation failed")
			case VideoStatusExpired:
				return nil, errors.New("grok: video generation request expired")
			case VideoStatusPending:
				continue
			}
		}
	}
}

// ============================================================================
// 视频编辑和扩展
// ============================================================================

// VideoEditRequest 视频编辑请求体。
type VideoEditRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Video  struct {
		URL string `json:"url"`
	} `json:"video"`
}

// EditVideo 编辑现有视频。视频生成仍为异步流程。
func (c *Client) EditVideo(ctx context.Context, model, prompt, videoURL string) (*VideoStartResponse, error) {
	req := VideoEditRequest{
		Model:  model,
		Prompt: prompt,
	}
	req.Video.URL = videoURL

	resp, err := c.newRequest("POST", "/v1/videos/edits").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result VideoStartResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// VideoExtendRequest 视频扩展请求体。
type VideoExtendRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Video  struct {
		URL string `json:"url"`
	} `json:"video"`
}

// ExtendVideo 从最后一帧扩展视频。视频生成仍为异步流程。
func (c *Client) ExtendVideo(ctx context.Context, model, prompt, videoURL string) (*VideoStartResponse, error) {
	req := VideoExtendRequest{
		Model:  model,
		Prompt: prompt,
	}
	req.Video.URL = videoURL

	resp, err := c.newRequest("POST", "/v1/videos/extensions").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result VideoStartResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// 确保 json 包被引用（用于将来可能的内联序列化辅助函数）。
var _ = json.Marshal
