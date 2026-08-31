//go:build linux || darwin

package cron

import (
	"os"
	"syscall"
)

// withFileLock 在写入 cron 存储文件时获取一把跨进程的咨询锁（flock），
// 防止多实例同时改写同一 JSON 文件造成更新丢失（报告 5325）。
// 锁文件与数据文件同目录、同名加 .lock 后缀；进程退出后由内核自动释放。
// 锁文件不可用时降级为无锁（单实例场景下无害）。
func withFileLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fn()
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
