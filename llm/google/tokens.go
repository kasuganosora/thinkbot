package google

import (
	"context"
	"errors"
)

// ============================================================================
// CountTokens
// ============================================================================

// CountTokens 统计消息的 token 数量。
func (c *Client) CountTokens(ctx context.Context, model string, req CountTokensRequest) (*CountTokensResponse, error) {
	if model == "" {
		return nil, errors.New("google: model is required")
	}
	if len(req.Contents) == 0 {
		return nil, errors.New("google: contents must not be empty")
	}

	resp, err := c.newRequest("POST", modelPath(model, "countTokens")).
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result CountTokensResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
