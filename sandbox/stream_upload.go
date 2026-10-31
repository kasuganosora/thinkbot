package sandbox

import (
	"errors"
	"io"
)

// ErrUserUploadTooLarge 表示用户上传超过调用方给定的上限。
// 工具写文件不走这条路径，仍由 MaxFileWrite 限制。
var ErrUserUploadTooLarge = errors.New("user upload exceeds size limit")

// CopyUserUpload 把 src 流式拷贝到 dst。
// limit > 0 时最多接受 limit 字节，多读到的 1 字节用来判定超限并返回 ErrUserUploadTooLarge。
// limit <= 0 表示不再加一道上限（调用方已经截断）。拷贝使用 io.Copy 的固定缓冲，不把全文装进内存。
func CopyUserUpload(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return io.Copy(dst, src)
	}
	lr := &byteLimitedReader{r: src, limit: limit}
	n, err := io.Copy(dst, lr)
	if lr.exceeded || errors.Is(err, ErrUserUploadTooLarge) {
		return n, ErrUserUploadTooLarge
	}
	return n, err
}

// byteLimitedReader 允许读到 limit+1，以便正好等于 limit 的文件成功，超过的失败。
type byteLimitedReader struct {
	r        io.Reader
	limit    int64
	n        int64
	exceeded bool
}

func (b *byteLimitedReader) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, ErrUserUploadTooLarge
	}
	room := b.limit - b.n + 1
	if room <= 0 {
		b.exceeded = true
		return 0, ErrUserUploadTooLarge
	}
	if int64(len(p)) > room {
		p = p[:room]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		b.exceeded = true
		return n, ErrUserUploadTooLarge
	}
	return n, err
}
