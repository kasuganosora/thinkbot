package grok

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strconv"

	httputil "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// Upload File — POST /v1/files (multipart/form-data)
// ============================================================================

// UploadFileParams 文件上传参数。
type UploadFileParams struct {
	// Filename 文件名（必须）。
	Filename string
	// Reader 文件内容读取器（必须）。
	Reader io.Reader
}

// UploadFile 上传文件。
func (c *Client) UploadFile(ctx context.Context, params UploadFileParams) (*FileInfo, error) {
	form := httputil.NewMultipartForm()
	form.AddFile("file", params.Filename, params.Reader)

	resp, err := c.newRequest("POST", "/v1/files").
		SetContext(ctx).
		SetMultipart(form).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result FileInfo
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// List Files — GET /v1/files
// ============================================================================

// ListFilesOptions 列表文件查询参数。
type ListFilesOptions struct {
	Limit           int    // 每页数量（默认 100）
	Order           string // "asc" 或 "desc"
	SortBy          string // "created_at", "filename", "size"
	PaginationToken string // 分页令牌
}

// ListFiles 列出已上传的文件。
func (c *Client) ListFiles(ctx context.Context, opts *ListFilesOptions) (*ListFilesResponse, error) {
	r := c.newRequest("GET", "/v1/files").SetContext(ctx)

	if opts != nil {
		if opts.Limit > 0 {
			r.SetQuery("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Order != "" {
			r.SetQuery("order", opts.Order)
		}
		if opts.SortBy != "" {
			r.SetQuery("sort_by", opts.SortBy)
		}
		if opts.PaginationToken != "" {
			r.SetQuery("pagination_token", opts.PaginationToken)
		}
	}

	resp, err := r.Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ListFilesResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Get File Metadata — GET /v1/files/{file_id}
// ============================================================================

// GetFile 获取文件元数据。
func (c *Client) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	resp, err := c.newRequest("GET", "/v1/files/"+url.PathEscape(fileID)).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result FileInfo
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Download File Content — GET /v1/files/{file_id}/content
// ============================================================================

// GetFileContent 下载文件内容，返回原始字节。
func (c *Client) GetFileContent(ctx context.Context, fileID string) ([]byte, error) {
	resp, err := c.newRequest("GET", "/v1/files/"+url.PathEscape(fileID)+"/content").
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	return resp.Body, nil
}

// GetFileContentReader 下载文件内容，返回 bytes.Reader。
func (c *Client) GetFileContentReader(ctx context.Context, fileID string) (*bytes.Reader, error) {
	data, err := c.GetFileContent(ctx, fileID)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// ============================================================================
// Delete File — DELETE /v1/files/{file_id}
// ============================================================================

// DeleteFile 删除文件。
func (c *Client) DeleteFile(ctx context.Context, fileID string) (*DeleteFileResponse, error) {
	resp, err := c.newRequest("DELETE", "/v1/files/"+url.PathEscape(fileID)).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result DeleteFileResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
