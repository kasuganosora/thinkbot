// Package idgen 提供带前缀的唯一 ID 生成能力。
//
// 基于 crypto/rand 生成随机字节，经 hex 编码后加上调用方指定的前缀，
// 适用于消息 ID、记忆条目 ID、笔记 ID 等场景。
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// randBytes 是随机 ID 的字节数（hex 编码后 24 字符）。
const randBytes = 12

// New 生成一个带前缀的唯一 ID。
//
// 格式为 "{prefix}-{24 hex chars}"。
// crypto/rand 失败时（极其罕见）回退到 "{prefix}-{unix-nano}"。
func New(prefix string) string {
	var buf [randBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf[:])
}
