package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// ============================================================================
// File API — 大文件上传与管理
//
// 对于总请求大小超过 20MB 的图片/音频/视频，需要先通过 File API 上传，
// 然后在 generateContent 请求中通过 fileUri 引用。
//
// 文档: https://ai.google.dev/gemini-api/docs/files
// ============================================================================

// File 表示 Files API 中的文件元数据。
type File struct {
	Name           string `json:"name"` // 资源名，如 "files/abc123"
	DisplayName    string `json:"displayName,omitempty"`
	MimeType       string `json:"mimeType,omitempty"`
	URI            string `json:"uri,omitempty"` // 在 generateContent 中使用的 fileUri
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	State          string `json:"state,omitempty"` // "FILE_STATE_ACTIVE" 等
	CreateTime     string `json:"createTime,omitempty"`
	UpdateTime     string `json:"updateTime,omitempty"`
	ExpirationTime string `json:"expirationTime,omitempty"`
}

// ListFilesResponse 文件列表响应。
type ListFilesResponse struct {
	Files         []File `json:"files"`
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// UploadFileOptions 文件上传选项。
type UploadFileOptions struct {
	// DisplayName 文件显示名（可选）。
	DisplayName string
	// MimeType 文件 MIME 类型（必填）。
	MimeType string
}

// ============================================================================
// UploadFile — 上传文件
// ============================================================================

// UploadFile 通过 Files API 上传文件（可恢复上传协议）。
//
// 适用场景：文件 > 20MB 或需要在多个请求中复用。
//
// 参数：
//   - ctx: context
//   - reader: 文件数据
//   - size: 文件字节数
//   - opts: 上传选项（DisplayName、MimeType）
//
// 返回上传后的 File 元数据，其 URI 字段可在 generateContent 中用作 fileUri。
func (c *Client) UploadFile(ctx context.Context, reader io.Reader, size int64, opts UploadFileOptions) (*File, error) {
	if opts.MimeType == "" {
		return nil, errors.New("google: mimeType is required for file upload")
	}

	// 步骤 1：发起可恢复上传请求，获取上传 URL
	startBody, _ := json.Marshal(map[string]any{
		"file": map[string]any{
			"displayName": opts.DisplayName,
		},
	})

	startResp, err := c.newRequest("POST", "/upload/v1beta/files").
		SetContext(ctx).
		SetHeader("X-Goog-Upload-Protocol", "resumable").
		SetHeader("X-Goog-Upload-Command", "start").
		SetHeader("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(size, 10)).
		SetHeader("X-Goog-Upload-Header-Content-Type", opts.MimeType).
		SetBody(bytes.NewReader(startBody)).
		Do()

	if err != nil {
		return nil, parseAPIError(startResp, err)
	}

	// 获取上传 URL
	uploadURL := startResp.Headers.Get("X-Goog-Upload-Url")
	if uploadURL == "" {
		return nil, errors.New("google: missing X-Goog-Upload-Url in upload start response")
	}

	// 步骤 2：上传实际文件数据
	// 上传 URL 是完整 URL，NewRequest 会直接使用它
	uploadResp, err := c.http.NewRequest("POST", uploadURL).
		SetContext(ctx).
		SetHeader("X-Goog-Upload-Command", "upload, finalize").
		SetHeader("X-Goog-Upload-Offset", "0").
		SetBody(reader).
		Do()

	if err != nil {
		return nil, parseAPIError(uploadResp, err)
	}

	var fileResp struct {
		File File `json:"file"`
	}
	if err := uploadResp.JSON(&fileResp); err != nil {
		return nil, err
	}

	return &fileResp.File, nil
}

// ============================================================================
// ListFiles — 列出文件
// ============================================================================

// ListFilesOptions 文件列表选项。
type ListFilesOptions struct {
	PageSize  int
	PageToken string
}

// ListFiles 列出已上传的文件。
func (c *Client) ListFiles(ctx context.Context, opts *ListFilesOptions) (*ListFilesResponse, error) {
	r := c.newRequest("GET", "/v1beta/files").SetContext(ctx)

	if opts != nil {
		if opts.PageSize > 0 {
			r.SetQuery("pageSize", strconv.Itoa(opts.PageSize))
		}
		if opts.PageToken != "" {
			r.SetQuery("pageToken", opts.PageToken)
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
// GetFile — 获取文件信息
// ============================================================================

// GetFile 获取单个文件的元数据。
//
// name 格式为 "files/abc123"。
func (c *Client) GetFile(ctx context.Context, name string) (*File, error) {
	if name == "" {
		return nil, errors.New("google: file name is required")
	}

	resp, err := c.newRequest("GET", "/v1beta/"+name).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result File
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// DeleteFile — 删除文件
// ============================================================================

// DeleteFile 删除一个已上传的文件。
//
// name 格式为 "files/abc123"。
func (c *Client) DeleteFile(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("google: file name is required")
	}

	resp, err := c.newRequest("DELETE", "/v1beta/"+name).
		SetContext(ctx).
		Do()
	if err != nil {
		return parseAPIError(resp, err)
	}
	return nil
}
