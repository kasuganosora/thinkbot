package loader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HealthStatus 是子进程 /health 探针的一次采样结果。
type HealthStatus struct {
	OK       bool
	Raw      map[string]any
	GitShort string
	Version  string
	PID      int
	Err      error
}

// HealthChecker 轮询子进程的 /health 端点。
type HealthChecker struct {
	URL    string
	Client *http.Client
}

// NewHealthChecker 构造检查器；单次请求超时固定 5s，避免长时间挂死。
func NewHealthChecker(url string) *HealthChecker {
	return &HealthChecker{
		URL:    url,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Check 立即探活一次。
func (h *HealthChecker) Check(ctx context.Context) HealthStatus {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return HealthStatus{Err: err}
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return HealthStatus{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return HealthStatus{Err: fmt.Errorf("health 返回状态码 %d", resp.StatusCode)}
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return HealthStatus{Err: err}
	}
	st := HealthStatus{OK: true, Raw: m}
	if v, ok := m["gitShort"].(string); ok {
		st.GitShort = v
	}
	if v, ok := m["version"].(string); ok {
		st.Version = v
	}
	if v, ok := m["pid"].(float64); ok {
		st.PID = int(v)
	}
	return st
}

// WaitUntilHealthy 周期性探活直到健康或 ctx 超时/取消。
func (h *HealthChecker) WaitUntilHealthy(ctx context.Context) HealthStatus {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		st := h.Check(ctx)
		if st.OK {
			return st
		}
		select {
		case <-ctx.Done():
			if st.Err == nil {
				st.Err = ctx.Err()
			}
			return st
		case <-ticker.C:
		}
	}
}
