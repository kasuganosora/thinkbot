package workflow

import (
	"context"
	"testing"
	"time"
)

func TestTaskWaitBudget(t *testing.T) {
	if got := taskWaitBudget(context.Background(), taskBlockingMaxTimeout); got != taskBlockingMaxTimeout {
		t.Fatalf("no deadline: want %v, got %v", taskBlockingMaxTimeout, got)
	}
	cases := []struct {
		left     time.Duration
		min, max time.Duration
	}{
		{time.Hour, taskBlockingMaxTimeout, taskBlockingMaxTimeout},                                            // 充裕：上限不变
		{15 * time.Minute, 15*time.Minute - taskReplyReserve - time.Second, 15*time.Minute - taskReplyReserve}, // 默认硬上限：留出回复时间
		{taskReplyReserve + 5*time.Second, 0, 0},                                                               // 不够阻塞：立即返回
	}
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), c.left)
		got := taskWaitBudget(ctx, taskBlockingMaxTimeout)
		cancel()
		if got < c.min || got > c.max {
			t.Errorf("deadline in %v: budget %v not in [%v, %v]", c.left, got, c.min, c.max)
		}
	}
}

// 默认 15 分钟硬上限下，task 至少还能阻塞一段有意义的时间，又给回复留足余量。
func TestTaskReplyReserveFitsDefaultHardTimeout(t *testing.T) {
	const defaultHardTimeout = 15 * time.Minute
	if taskReplyReserve >= defaultHardTimeout/2 {
		t.Fatalf("reserve %v eats more than half of the default hard timeout", taskReplyReserve)
	}
	if taskReplyReserve < time.Minute {
		t.Fatalf("reserve %v leaves too little time for the final reply", taskReplyReserve)
	}
}
