package inbound

import (
	"sync"
	"testing"
)

func TestSelfIDSet_Basic(t *testing.T) {
	s := NewSelfIDSet()

	// 空 set 不包含任何 ID
	if s.Contains("anyone") {
		t.Error("empty set should not contain any ID")
	}
	if s.Len() != 0 {
		t.Errorf("empty set Len = %d, want 0", s.Len())
	}

	// 添加 ID
	s.Add("bot-123")
	s.Add("bot-tg")
	if !s.Contains("bot-123") {
		t.Error("should contain bot-123")
	}
	if !s.Contains("bot-tg") {
		t.Error("should contain bot-tg")
	}
	if s.Contains("user-1") {
		t.Error("should not contain user-1")
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}

	// 重复添加不会增加
	s.Add("bot-123")
	if s.Len() != 2 {
		t.Errorf("duplicate Add should not increase Len, got %d", s.Len())
	}

	// 移除
	s.Remove("bot-123")
	if s.Contains("bot-123") {
		t.Error("should not contain bot-123 after Remove")
	}
	if !s.Contains("bot-tg") {
		t.Error("should still contain bot-tg")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestSelfIDSet_EmptyIgnored(t *testing.T) {
	s := NewSelfIDSet()
	s.Add("")
	if s.Len() != 0 {
		t.Error("empty string Add should be ignored")
	}
	if s.Contains("") {
		t.Error("Contains('') should always be false")
	}
}

func TestSelfIDSet_Concurrent(t *testing.T) {
	s := NewSelfIDSet()
	var wg sync.WaitGroup

	// 并发写入
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Add("bot-" + itoa(n))
		}(i)
	}

	// 并发读取
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Contains("bot-" + itoa(n))
		}(i)
	}

	wg.Wait()

	if s.Len() != 100 {
		t.Errorf("after concurrent Add, Len = %d, want 100", s.Len())
	}
}

// itoa 简单整数转字符串，避免引入 strconv
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
