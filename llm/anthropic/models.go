package anthropic

import (
	"context"
	"net/url"
	"strconv"
)

// ============================================================================
// ListModels
// ============================================================================

// ListModelsOptions 列表查询参数。
type ListModelsOptions struct {
	// Limit 每页数量（1-1000），默认 20。
	Limit int
	// BeforeID 返回 ID 小于此值的记录（向前翻页）。
	BeforeID string
	// AfterID 返回 ID 大于此值的记录（向后翻页）。
	AfterID string
}

// ListModels 获取可用模型列表。
func (c *Client) ListModels(ctx context.Context, opts *ListModelsOptions) (*ListModelsResponse, error) {
	r := c.newRequest("GET", "/v1/models").SetContext(ctx)

	if opts != nil {
		if opts.Limit > 0 {
			r.SetQuery("limit", strconv.Itoa(opts.Limit))
		}
		if opts.BeforeID != "" {
			r.SetQuery("before_id", opts.BeforeID)
		}
		if opts.AfterID != "" {
			r.SetQuery("after_id", opts.AfterID)
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
func (c *Client) GetModel(ctx context.Context, modelID string) (*Model, error) {
	resp, err := c.newRequest("GET", "/v1/models/"+url.PathEscape(modelID)).
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
