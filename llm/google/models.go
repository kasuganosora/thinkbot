package google

import (
	"context"
	"errors"
	"strconv"
)

// ============================================================================
// ListModels
// ============================================================================

// ListModelsOptions 列表查询参数。
type ListModelsOptions struct {
	// PageSize 每页数量（1-100），默认 50。
	PageSize int
	// PageToken 翻页令牌（来自上一次响应的 nextPageToken）。
	PageToken string
	// Filter 过滤条件（如 "supportsGenerateContent=true"）。
	Filter string
}

// ListModels 获取可用模型列表。
func (c *Client) ListModels(ctx context.Context, opts *ListModelsOptions) (*ListModelsResponse, error) {
	r := c.newRequest("GET", "/v1beta/models").SetContext(ctx)

	if opts != nil {
		if opts.PageSize > 0 {
			r.SetQuery("pageSize", strconv.Itoa(opts.PageSize))
		}
		if opts.PageToken != "" {
			r.SetQuery("pageToken", opts.PageToken)
		}
		if opts.Filter != "" {
			r.SetQuery("filter", opts.Filter)
		}
	}

	resp, err := r.Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ListModelsResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetModel 获取单个模型的详细信息。
//
// modelID 可以是 "gemini-2.5-flash" 或 "models/gemini-2.5-flash"。
func (c *Client) GetModel(ctx context.Context, modelID string) (*Model, error) {
	if modelID == "" {
		return nil, errors.New("google: model ID is required")
	}
	// 规范化：确保以 models/ 开头
	if len(modelID) < 7 || modelID[:7] != "models/" {
		modelID = "models/" + modelID
	}

	resp, err := c.newRequest("GET", "/v1beta/"+modelID).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result Model
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
