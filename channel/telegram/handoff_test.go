package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
)

func TestRunHandoff_NoOverlapAndNoReprocess(t *testing.T) {
	var active atomic.Int32
	var maxSeen atomic.Int32
	var phase atomic.Int32
	var mu sync.Mutex
	returned := []int64{}
	note := func(n int32) {
		for {
			seen := maxSeen.Load()
			if n <= seen || maxSeen.CompareAndSwap(seen, n) {
				return
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":7,"is_bot":true,"username":"synth"}}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(body, &req)
		n := active.Add(1)
		note(n)
		defer active.Add(-1)
		id := int64(0)
		switch {
		case req.Offset == 0 && phase.Load() == 0:
			id = 10
		case req.Offset == 11 && phase.Load() == 1:
			id = 11
		case req.Offset == 12 && phase.Load() == 2:
			id = 12
		}
		if id == 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		mu.Lock()
		returned = append(returned, id)
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":%d,"message":{"message_id":1,"text":"hi","chat":{"id":1,"type":"private"},"from":{"id":2}}}]}`, id)
	}))
	defer srv.Close()

	logger := zap.NewNop().Sugar()
	tp := nooptrace.NewTracerProvider()
	mp := noop.NewMeterProvider()
	newBot := func(name string) (*bot.Bot, *TelegramChannel) {
		ch := NewChannel(name, "bot-synth", Config{
			Token: "123456:TEST", APIBaseURL: srv.URL, PollTimeout: 1, StreamReply: false,
		})
		p, err := pipeline.New(nil, tp, mp, logger)
		if err != nil {
			t.Fatal(err)
		}
		b, err := bot.New(bot.BotParams{
			ID: name, Logger: logger, TP: tp,
			Dispatcher: outbound.NewMultiDispatcher(logger, tp),
			Pipeline:   p, Channels: []bot.Channel{ch},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b, ch
	}
	oldBot, oldCh := newBot("old")
	newRun, newCh := newBot("new")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = oldBot.Run(ctx) }()
	waitUntil(t, 3*time.Second, func() bool {
		return containsID(oldCh.HandledUpdateIDs(), 10) && active.Load() >= 1
	})
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := oldBot.DrainPolling(drainCtx); err != nil {
		drainCancel()
		t.Fatal(err)
	}
	drainCancel()
	waitUntil(t, 2*time.Second, func() bool { return active.Load() == 0 })
	if got := oldBot.PollingOffset(); got != 11 {
		t.Fatalf("offset %d", got)
	}
	newRun.SetPollingOffset(oldBot.PollingOffset())
	phase.Store(1)
	go func() { _ = newRun.Run(ctx) }()
	waitUntil(t, 3*time.Second, func() bool { return containsID(newCh.HandledUpdateIDs(), 11) })
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = newRun.StopAndWait(stopCtx)
	stopCancel()
	waitUntil(t, 2*time.Second, func() bool { return active.Load() == 0 })
	phase.Store(2)
	oldBot.AdoptNewerOffset(newRun)
	if err := oldBot.ResumePolling(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool { return containsID(oldCh.HandledUpdateIDs(), 12) })
	if maxSeen.Load() > 1 {
		t.Fatalf("overlapping getUpdates: %d", maxSeen.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[int64]int{}
	for _, id := range returned {
		seen[id]++
	}
	if seen[10] != 1 || seen[11] != 1 || seen[12] != 1 {
		t.Fatalf("updates delivered %#v", returned)
	}
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
