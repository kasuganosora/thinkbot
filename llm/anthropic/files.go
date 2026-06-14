package anthropic

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
	// MimeType 文件 MIME 类型（可选，不设则由 server 推断）。
	MimeType string
	// Reader 文件内容读取器（必须）。
	Reader io.Reader
}

// UploadFile 上传文件。需要 beta header（自动添加）。
func (c *Client) UploadFile(ctx context.Context, params UploadFileParams) (*FileMetadata, error) {
	form := httputil.NewMultipartForm()
	if params.MimeType != "" {
		form.AddFileWithMIME("file", params.Filename, params.MimeType, params.Reader)
	} else {
		form.AddFile("file", params.Filename, params.Reader)
	}

	resp, err := c.newRequest("POST", "/v1/files").
		SetContext(ctx).
		SetHeader("anthropic-beta", BetaFilesAPI).
		SetMultipart(form).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result FileMetadata
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
	// AfterID 返回此 ID 之后的记录（向后翻页）。
	AfterID string
	// BeforeID 返回此 ID 之前的记录（向前翻页）。
	BeforeID string
	// Limit 每页数量（1-1000），默认 20。
	Limit int
	// ScopeID 按 scope ID 过滤（如 session ID）。
	ScopeID string
}

// ListFiles 列出已上传的文件。需要 beta header（自动添加）。
func (c *Client) ListFiles(ctx context.Context, opts *ListFilesOptions) (*ListFilesResponse, error) {
	r := c.newRequest("GET", "/v1/files").
		SetContext(ctx).
		SetHeader("anthropic-beta", BetaFilesAPI)

	if opts != nil {
		if opts.Limit > 0 {
			r.SetQuery("limit", strconv.Itoa(opts.Limit))
		}
		if opts.AfterID != "" {
			r.SetQuery("after_id", opts.AfterID)
		}
		if opts.BeforeID != "" {
			r.SetQuery("before_id", opts.BeforeID)
		}
		if opts.ScopeID != "" {
			r.SetQuery("scope_id", opts.ScopeID)
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
// Download File — GET /v1/files/{file_id}/content
// ============================================================================

// DownloadFile 下载文件内容，返回原始字节和 Content-Type。
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	resp, err := c.newRequest("GET", "/v1/files/"+url.PathEscape(fileID)+"/content").
		SetContext(ctx).
		SetHeader("anthropic-beta", BetaFilesAPI).
		Do()
	if err != nil {
		return nil, "", parseAPIError(resp, err)
	}

	contentType := resp.Headers.Get("Content-Type")
	return resp.Body, contentType, nil
}

// DownloadFileReader 下载文件内容，回调 io.Reader 供流式处理。
//
// 注意：底层 HTTP 客户端会将响应体完整读入内存后返回 []byte。
// 如需真正的流式下载，建议直接使用 httputil.Client。
func (c *Client) DownloadFileReader(ctx context.Context, fileID string) (*bytes.Reader, string, error) {
	data, contentType, err := c.DownloadFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), contentType, nil
}

// ============================================================================
// Get File Metadata — GET /v1/files/{file_id}
// ============================================================================

// GetFileMetadata 获取文件元数据。需要 beta header（自动添加）。
func (c *Client) GetFileMetadata(ctx context.Context, fileID string) (*FileMetadata, error) {
	resp, err := c.newRequest("GET", "/v1/files/"+url.PathEscape(fileID)).
		SetContext(ctx).
		SetHeader("anthropic-beta", BetaFilesAPI).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result FileMetadata
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Delete File — DELETE /v1/files/{file_id}
// ============================================================================

// DeleteFile 删除文件。需要 beta header（自动添加）。
func (c *Client) DeleteFile(ctx context.Context, fileID string) (*DeletedFile, error) {
	resp, err := c.newRequest("DELETE", "/v1/files/"+url.PathEscape(fileID)).
		SetContext(ctx).
		SetHeader("anthropic-beta", BetaFilesAPI).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result DeletedFile
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
