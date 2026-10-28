package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// 用户上传与工具写入分开限流。
//
// sandbox.Config.MaxFileWrite（默认 10MB）继续约束模型/工具写文件。
// 用户上传走流式拷贝，上限与已部署的网关 client_max_body_size（1000m）对齐，
// 应用不再把正文读进内存，也不再套用 10MB 的工具写入上限。
const (
	// MaxUserUploadBytes 是会话工作区与 Bot 文件面板的单文件上限（1000 MiB）。
	// 与网关 client_max_body_size 1000m 一致；反向代理不用为这次改动再调大。
	// multipart 的 boundary 与 path 字段由 maxUploadBodyBytes 另留 1MB。
	MaxUserUploadBytes int64 = 1000 << 20

	// MaxChatAttachmentBytes 是聊天输入框单个附件的原始字节上限。
	// 附件以 base64 data URL 放进 POST /api/chat/send 的 JSON，不经过工作区流式上传，
	// 因此不受 MaxFileWrite 的 10MB 约束，也不走下面的 1000MB 工作区通道。
	MaxChatAttachmentBytes int64 = 20 << 20
	// MaxChatAttachmentTotalBytes 是同一次发送的附件原始字节合计上限。
	MaxChatAttachmentTotalBytes int64 = 60 << 20
	// MaxChatAttachmentCount 是同一次发送的附件个数上限。
	MaxChatAttachmentCount = 10
)

// maxUploadBodyBytes 是 multipart 请求体上限：文件本体 + 表单开销。
func maxUploadBodyBytes() int64 {
	return MaxUserUploadBytes + (1 << 20)
}

func uploadTooLargeError() error {
	return errs.BadRequest(fmt.Sprintf("file exceeds the %d MB upload limit", MaxUserUploadBytes>>20))
}

func (s *Server) workspaceForUpload(botID string) (sandbox.Workspace, bool) {
	if s != nil && s.uploadWorkspaceHook != nil {
		return s.uploadWorkspaceHook(botID)
	}
	return s.botFileWorkspace(botID)
}

func requestContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

type userUploadStreamer interface {
	WriteUserUploadStream(ctx context.Context, path string, r io.Reader, limit int64) (int64, error)
}

func writeUploadedWorkspaceStream(ctx context.Context, ws sandbox.Workspace, path string, r io.Reader, limit int64) (int64, error) {
	up, ok := ws.(userUploadStreamer)
	if !ok {
		return 0, errs.Internal("workspace does not support streaming upload")
	}
	return up.WriteUserUploadStream(ctx, path, r, limit)
}

// consumeWorkspaceUpload 从 multipart 请求里流式取出一个文件并写入工作区。
// path 必须出现在 file 之前：持久容器的 named volume 在主进程里不可见，
// 不能先把正文落到宿主临时文件再 docker cp。file 部分会直接拷进
// `docker exec -i … cat > path` 的 stdin（local 后端则 io.Copy 到宿主工作区文件）。
func (s *Server) consumeWorkspaceUpload(c *gin.Context, botID string) (path, name string, n int64, err error) {
	if err = prepareUploadBody(c); err != nil {
		return "", "", 0, err
	}
	mr, err := c.Request.MultipartReader()
	if err != nil {
		if isRequestBodyTooLarge(err) {
			return "", "", 0, uploadTooLargeError()
		}
		return "", "", 0, errs.BadRequest("file is required: " + err.Error())
	}

	dir := "/"
	havePath := false
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			if isRequestBodyTooLarge(perr) {
				return "", "", 0, uploadTooLargeError()
			}
			return "", "", 0, errs.BadRequest("file is required: " + perr.Error())
		}
		switch part.FormName() {
		case "path":
			b, rerr := io.ReadAll(io.LimitReader(part, 8<<10))
			_ = part.Close()
			if rerr != nil {
				return "", "", 0, errs.BadRequest("invalid path")
			}
			dir = strings.TrimSpace(string(b))
			if dir == "" {
				dir = "/"
			}
			havePath = true
		case "file":
			if !havePath {
				_ = part.Close()
				return "", "", 0, errs.BadRequest("path field must precede file")
			}
			fileName := part.FileName()
			n, err = s.streamWorkspaceFile(c, botID, dir, fileName, part, MaxUserUploadBytes)
			_ = part.Close()
			if err != nil {
				return "", "", 0, err
			}
			return dir, fileName, n, nil
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
			_ = part.Close()
		}
	}
	return "", "", 0, errs.BadRequest("file is required")
}

func prepareUploadBody(c *gin.Context) error {
	if c.Request == nil || c.Request.Body == nil {
		return errs.BadRequest("file is required")
	}
	if c.Request.ContentLength > maxUploadBodyBytes() {
		return uploadTooLargeError()
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBodyBytes())
	return nil
}

func isRequestBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

func mapUploadWriteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sandbox.ErrUserUploadTooLarge) || isRequestBodyTooLarge(err) {
		return uploadTooLargeError()
	}
	var ee *errs.Error
	if errors.As(err, &ee) && ee.Code() != 0 {
		return err
	}
	return errs.Internal("failed to write file: " + err.Error())
}

// streamWorkspaceFile 将 r 流式写入工作区。limit 是文件字节上限。
func (s *Server) streamWorkspaceFile(c *gin.Context, botID, path, name string, content io.Reader, limit int64) (int64, error) {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return 0, errs.BadRequest("invalid file name")
	}

	if ws, ok := s.workspaceForUpload(botID); ok {
		target := filepath.ToSlash(filepath.Join(path, name))
		n, err := writeUploadedWorkspaceStream(requestContext(c), ws, target, content, limit)
		if err != nil {
			return 0, mapUploadWriteError(err)
		}
		return n, nil
	}

	root, err := s.botWorkspaceRoot(botID)
	if err != nil {
		return 0, errs.Internal("failed to resolve workspace root: " + err.Error())
	}
	dir, err := safeJoin(root, path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, errs.Internal("failed to prepare directory: " + err.Error())
	}
	target, err := safeJoin(root, path, name)
	if err != nil {
		return 0, err
	}
	n, err := writeHostStream(target, content, limit)
	if err != nil {
		return 0, mapUploadWriteError(err)
	}
	return n, nil
}

func writeHostStream(target string, r io.Reader, limit int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := sandbox.CopyUserUpload(f, r, limit)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		if copyErr != nil {
			return n, copyErr
		}
		return n, closeErr
	}
	return n, nil
}

func validateChatAttachments(atts []ChatAttachment) error {
	if len(atts) > MaxChatAttachmentCount {
		return errs.BadRequest(fmt.Sprintf("too many attachments (max %d)", MaxChatAttachmentCount))
	}
	var total int64
	for _, a := range atts {
		n := attachmentBytes(a)
		if n > MaxChatAttachmentBytes {
			name := a.Name
			if name == "" {
				name = "attachment"
			}
			return errs.BadRequest(fmt.Sprintf("attachment %q exceeds the %d MB limit", name, MaxChatAttachmentBytes>>20))
		}
		total += n
		if total > MaxChatAttachmentTotalBytes {
			return errs.BadRequest(fmt.Sprintf("attachments exceed the %d MB total limit", MaxChatAttachmentTotalBytes>>20))
		}
	}
	return nil
}

func attachmentBytes(a ChatAttachment) int64 {
	n := a.Size
	if decoded := dataURLDecodedSize(a.DataUrl); decoded > n {
		n = decoded
	}
	if n < 0 {
		return 0
	}
	return n
}

func dataURLDecodedSize(dataURL string) int64 {
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 || comma+1 >= len(dataURL) {
		return 0
	}
	payload := dataURL[comma+1:]
	n := len(payload)
	if n == 0 {
		return 0
	}
	pad := 0
	if strings.HasSuffix(payload, "==") {
		pad = 2
	} else if strings.HasSuffix(payload, "=") {
		pad = 1
	}
	decoded := n/4*3 - pad
	if decoded < 0 {
		return 0
	}
	return int64(decoded)
}
