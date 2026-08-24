//go:build !linux && !darwin

package cron

// withFileLock 在非 Unix 平台（如 Windows）无 syscall.Flock，退化为直接执行。
// 多实例并发写同一文件的场景在 Windows 部署下不属于支持范围。
func withFileLock(path string, fn func() error) error {
	return fn()
}
