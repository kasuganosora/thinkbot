package searchproviders

import (
	"fmt"
	"sync"
	"time"
)

const (
	authCooldown = 10 * time.Minute
	rateCooldown = 60 * time.Second
)

// 进程内熔断：401/403 跳过 10 分钟，429 跳过 Retry-After（默认 60 秒）。
// 超时/5xx/网络错误不走这里。Store.Save 成功后清空。
type circuitEntry struct {
	until  time.Time
	reason string
}

var (
	circuitMu sync.Mutex
	circuits  = map[string]circuitEntry{}
	now       = time.Now
)

func clearCircuits() {
	circuitMu.Lock()
	defer circuitMu.Unlock()
	circuits = map[string]circuitEntry{}
}

func clearCircuit(id string) {
	if id == "" {
		return
	}
	circuitMu.Lock()
	defer circuitMu.Unlock()
	delete(circuits, id)
}

func tripAuthCircuit(id string) {
	if id == "" {
		return
	}
	circuitMu.Lock()
	defer circuitMu.Unlock()
	circuits[id] = circuitEntry{until: now().Add(authCooldown), reason: "HTTP 401/403"}
}

func tripRateCircuit(id string, d time.Duration) {
	if id == "" {
		return
	}
	if d <= 0 {
		d = rateCooldown
	}
	circuitMu.Lock()
	defer circuitMu.Unlock()
	circuits[id] = circuitEntry{until: now().Add(d), reason: "HTTP 429"}
}

func circuitSkipReason(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	circuitMu.Lock()
	defer circuitMu.Unlock()
	e, ok := circuits[id]
	if !ok {
		return "", false
	}
	if !now().Before(e.until) {
		delete(circuits, id)
		return "", false
	}
	return fmt.Sprintf("skipped: %s cooldown", e.reason), true
}
