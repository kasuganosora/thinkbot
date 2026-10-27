package storage

import (
	"testing"
	"time"
)

func TestAccessToucherCoalescesWithinInterval(t *testing.T) {
	tch := &accessToucher{interval: 10 * time.Minute}
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

	first := tch.claim([]string{"a", "b"}, now)
	if len(first) != 2 || first[0] != "a" || first[1] != "b" {
		t.Fatalf("first claim = %v want [a b]", first)
	}

	second := tch.claim([]string{"b", "a", "c"}, now.Add(time.Minute))
	if len(second) != 1 || second[0] != "c" {
		t.Fatalf("second claim = %v want [c]", second)
	}

	// c 在 +1min 才认领，+10min 时还差一分钟；+11min 时 a 和 c 都到期。
	third := tch.claim([]string{"a", "c"}, now.Add(11*time.Minute))
	if len(third) != 2 || third[0] != "a" || third[1] != "c" {
		t.Fatalf("third claim = %v want [a c]", third)
	}
}

func TestAccessToucherDefaultIntervalStillCoalesces(t *testing.T) {
	var tch accessToucher
	now := time.Now()
	if got := tch.claim([]string{"x"}, now); len(got) != 1 {
		t.Fatalf("first = %v", got)
	}
	if got := tch.claim([]string{"x"}, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("repeat inside default interval = %v want none", got)
	}
}
