package interaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// validQuestion 构造一个合法的测试问题。
func validQuestion(id string) Question {
	return Question{
		ID:       id,
		BotID:    "bot-1",
		ChatID:   "chat-1",
		Question: "选择一个",
		Options: []Option{
			{Label: "甲"}, {Label: "乙"}, {Label: "丙"},
		},
		Mode: ModeSingle,
	}
}

func TestRegisterQuestionValidation(t *testing.T) {
	r := NewRegistry()

	cases := []struct {
		name   string
		mutate func(q *Question)
		want   error
	}{
		{"缺 ID", func(q *Question) { q.ID = "" }, ErrInvalidQuestion},
		{"缺正文", func(q *Question) { q.Question = "" }, ErrInvalidQuestion},
		{"零选项", func(q *Question) { q.Options = nil }, ErrInvalidOptions},
		{"九个选项", func(q *Question) {
			q.Options = make([]Option, 9)
			for i := range q.Options {
				q.Options[i] = Option{Label: fmt.Sprint(i)}
			}
		}, ErrInvalidOptions},
		{"选项缺 label", func(q *Question) {
			q.Options = []Option{{Label: "甲"}, {Label: ""}}
		}, ErrInvalidQuestion},
		{"非法 mode", func(q *Question) { q.Mode = Mode("both") }, ErrInvalidMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validQuestion("q-" + tc.name)
			tc.mutate(&q)
			_, err := r.RegisterQuestion(q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestRegisterDuplicateID(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterQuestion(validQuestion("dup")); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("want ErrDuplicateID, got %v", err)
	}
}

func TestRegisterDefaults(t *testing.T) {
	r := NewRegistry()
	q := validQuestion("defaults")
	q.TimeoutSecs = 0
	reg, err := r.RegisterQuestion(q)
	if err != nil {
		t.Fatal(err)
	}
	if reg.TimeoutSecs != DefaultTimeoutSecs {
		t.Fatalf("timeout not defaulted: %d", reg.TimeoutSecs)
	}
	if reg.Status != StatusPending {
		t.Fatalf("status not pending: %s", reg.Status)
	}
}

func TestWaitResolve(t *testing.T) {
	r := NewRegistry()
	q := validQuestion("wr")
	if _, err := r.RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := r.Resolve("wr", Answer{Selected: []int{1}, Via: ViaWeb}); err != nil {
			t.Errorf("resolve: %v", err)
		}
	}()

	snap, ans, err := r.Wait(context.Background(), "wr")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != StatusAnswered || len(ans.Selected) != 1 || ans.Selected[0] != 1 {
		t.Fatalf("bad answer: %+v %+v", snap, ans)
	}
}

func TestResolveValidation(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("rv")); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve("rv", Answer{Selected: []int{5}, Via: ViaWeb}); !errors.Is(err, ErrInvalidSelected) {
		t.Fatalf("want ErrInvalidSelected(越界), got %v", err)
	}
	if err := r.Resolve("rv", Answer{Selected: []int{0, 1}, Via: ViaWeb}); !errors.Is(err, ErrInvalidSelected) {
		t.Fatalf("want ErrInvalidSelected(单选多个), got %v", err)
	}
	if err := r.Resolve("rv", Answer{Selected: []int{0}, Via: Via("sms")}); !errors.Is(err, ErrInvalidVia) {
		t.Fatalf("want ErrInvalidVia, got %v", err)
	}
	if err := r.Resolve("rv", Answer{Via: ViaWeb}); !errors.Is(err, ErrInvalidSelected) {
		t.Fatalf("want ErrInvalidSelected(空应答), got %v", err)
	}
	// 校验失败不得改变状态。
	snap, _ := r.Lookup("rv")
	if snap.Status != StatusPending {
		t.Fatalf("failed resolve changed status: %s", snap.Status)
	}
}

func TestTimeout(t *testing.T) {
	r := NewRegistry()
	q := validQuestion("to")
	q.TimeoutSecs = 1 // 1 秒超时（保持测试轻量）
	if _, err := r.RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err := r.Wait(context.Background(), "to")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("timeout fired too early: %v", elapsed)
	}
	// 超时后应为终态，Resolve 报错。
	if err := r.Resolve("to", Answer{Selected: []int{0}, Via: ViaWeb}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("want ErrAlreadyResolved after timeout, got %v", err)
	}
}

func TestConcurrentResolveOnlyOneWins(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("cr")); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var okCount, errCount int64
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Resolve("cr", Answer{Selected: []int{0}, Via: ViaWeb})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
			} else if errors.Is(err, ErrAlreadyResolved) {
				errCount++
			}
		}()
	}
	wg.Wait()
	if okCount != 1 || errCount != n-1 {
		t.Fatalf("want 1 success / %d already-resolved, got %d/%d", n-1, okCount, errCount)
	}
}

func TestWaitCtxCancel(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("wc")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := r.Wait(ctx, "wc")
	if err == nil {
		t.Fatal("want error on ctx cancel")
	}
	// ctx 取消应连带把问题置为终态。
	snap, _ := r.Lookup("wc")
	if snap.Status == StatusPending {
		t.Fatal("question still pending after ctx cancel")
	}
}

func TestCancel(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("cc")); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = r.Cancel("cc")
	}()
	_, _, err := r.Wait(context.Background(), "cc")
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if err := r.Cancel("cc"); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("double cancel want ErrAlreadyResolved, got %v", err)
	}
}

func TestLookupNotFound(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Lookup("ghost"); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatalf("want ErrQuestionNotFound, got %v", err)
	}
}

func TestCleanupFinal(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("cf")); err != nil {
		t.Fatal(err)
	}
	// pending 不能清理。
	r.CleanupFinal("cf")
	if _, err := r.Lookup("cf"); err != nil {
		t.Fatalf("pending question must not be cleaned: %v", err)
	}
	_ = r.Resolve("cf", Answer{Selected: []int{0}, Via: ViaMisskey})
	r.CleanupFinal("cf")
	if _, err := r.Lookup("cf"); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatalf("final question should be removed, got %v", err)
	}
}

func TestOptionsIsolation(t *testing.T) {
	r := NewRegistry()
	q := validQuestion("oi")
	if _, err := r.RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	// 调用方改自己的切片不应影响注册表。
	q.Options[0].Label = "篡改"
	snap, _ := r.Lookup("oi")
	if snap.Options[0].Label == "篡改" {
		t.Fatal("options slice leaked to caller")
	}
}

func TestLookupCopiesUnderLock(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("lk-copy")); err != nil {
		t.Fatal(err)
	}
	snap, err := r.Lookup("lk-copy")
	if err != nil {
		t.Fatal(err)
	}
	snap.Options[0].Label = "篡改 Lookup 返回值"
	snap.Status = StatusAnswered
	again, err := r.Lookup("lk-copy")
	if err != nil {
		t.Fatal(err)
	}
	if again.Options[0].Label == "篡改 Lookup 返回值" {
		t.Fatal("Lookup returned a shared Options slice")
	}
	if again.Status != StatusPending {
		t.Fatalf("Lookup snapshot Status leaked mutation: %s", again.Status)
	}

	// concurrent Resolve must not race with Lookup (run with -race)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s, _ := r.Lookup("lk-copy")
			_ = s.Status
			if len(s.Options) > 0 {
				s.Options[0].Label = "x"
			}
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		_ = r.Resolve("lk-copy", Answer{Selected: []int{0}, Via: ViaWeb})
	}()
	wg.Wait()
	final, err := r.Lookup("lk-copy")
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusAnswered {
		t.Fatalf("status after concurrent Lookup/Resolve = %s", final.Status)
	}
}

func TestResolveFromWrongChatID(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("rf-wrong")); err != nil {
		t.Fatal(err)
	}
	err := r.ResolveFrom("rf-wrong", "other-chat", Answer{Selected: []int{0}, Via: ViaWeb})
	if !errors.Is(err, ErrQuestionNotFound) {
		t.Fatalf("want ErrQuestionNotFound, got %v", err)
	}
	snap, _ := r.Lookup("rf-wrong")
	if snap.Status != StatusPending {
		t.Fatalf("wrong-chat ResolveFrom mutated status: %s", snap.Status)
	}

	// concurrent rightful Resolve, then a stolen-chat ResolveFrom must still reject
	var wg sync.WaitGroup
	wg.Add(2)
	var rightErr, stolenErr error
	go func() {
		defer wg.Done()
		rightErr = r.Resolve("rf-wrong", Answer{Selected: []int{1}, Via: ViaWeb})
	}()
	go func() {
		defer wg.Done()
		stolenErr = r.ResolveFrom("rf-wrong", "stolen-chat", Answer{Selected: []int{0}, Via: ViaWeb})
	}()
	wg.Wait()
	if rightErr != nil && !errors.Is(rightErr, ErrAlreadyResolved) {
		t.Fatalf("rightful Resolve: %v", rightErr)
	}
	if stolenErr == nil {
		t.Fatal("stolen-chat ResolveFrom succeeded")
	}
	if !errors.Is(stolenErr, ErrQuestionNotFound) && !errors.Is(stolenErr, ErrAlreadyResolved) {
		t.Fatalf("stolen-chat err = %v", stolenErr)
	}
	snap, _ = r.Lookup("rf-wrong")
	if snap.Status != StatusAnswered {
		t.Fatalf("status = %s, want answered", snap.Status)
	}
}

func TestResolveFromMatchingChatID(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("rf-ok")); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveFrom("rf-ok", "chat-1", Answer{Selected: []int{2}, Via: ViaTelegram}); err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Lookup("rf-ok")
	if snap.Status != StatusAnswered {
		t.Fatalf("status = %s", snap.Status)
	}
}

func TestAbortPending(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("ab")); err != nil {
		t.Fatal(err)
	}
	r.AbortPending("ab")
	if _, err := r.Lookup("ab"); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatalf("AbortPending should cancel+remove, got %v", err)
	}
	if n := r.PendingCount(); n != 0 {
		t.Fatalf("pending after AbortPending = %d", n)
	}
}
