package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// generateID 生成一个带前缀的唯一 ID。
func generateID(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf[:])
}
