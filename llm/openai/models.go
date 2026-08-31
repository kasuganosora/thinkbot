package openai

import "context"

// ============================================================================
// Models
// ============================================================================

// ListModels 列出当前可用的所有模型。
func (c *Client) ListModels(ctx context.Context) (*ListModelsResponse, error) {
	resp, err := c.newRequest("GET", "/v1/models").
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ListModelsResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RetrieveModel 获取单个模型信息。
func (c *Client) RetrieveModel(ctx context.Context, modelID string) (*Model, error) {
	resp, err := c.newRequest("GET", "/v1/models/"+modelID).
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

// DeleteModel 删除一个微调模型。
func (c *Client) DeleteModel(ctx context.Context, modelID string) error {
	resp, err := c.newRequest("DELETE", "/v1/models/"+modelID).
		SetContext(ctx).
		Do()
	if err != nil {
		return parseAPIError(resp, err)
	}
	return nil
}

// FindModel 在模型列表中查找指定 ID 的模型。
func (r *ListModelsResponse) FindModel(id string) *Model {
	for i := range r.Data {
		if r.Data[i].ID == id {
			return &r.Data[i]
		}
	}
	return nil
}
