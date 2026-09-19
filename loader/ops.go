package loader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/internal/buildinfo"
	"go.uber.org/zap"
)

// OpsServer 暴露 loader 的运维 HTTP 接口（独立于 thinkbot 主 API，默认 127.0.0.1:8090）。
// 变更类接口受 loader.token 保护（X-Loader-Token 或 Bearer），/loader/health 公开供探针使用。
type OpsServer struct {
	cfg Config
	log *zap.SugaredLogger
	sup *Supervisor
	dep *Deployer
	srv *http.Server
}

// NewOpsServer 构造运维服务。
func NewOpsServer(cfg Config, log *zap.SugaredLogger, sup *Supervisor, dep *Deployer) *OpsServer {
	return &OpsServer{cfg: cfg, log: log, sup: sup, dep: dep}
}

// auth 中间件：校验 loader.token。token 为空直接 503（fail-safe），不匹配 401。
func (o *OpsServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o.cfg.Token == "" {
			http.Error(w, "ops disabled: token not configured", http.StatusServiceUnavailable)
			return
		}
		tok := r.Header.Get("X-Loader-Token")
		if tok == "" {
			if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
				tok = strings.TrimPrefix(ah, "Bearer ")
			}
		}
		if tok != o.cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// Start 启动 HTTP 服务（阻塞）。
func (o *OpsServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/loader/health", o.handleHealth)
	mux.HandleFunc("/loader/status", o.auth(o.handleStatus))
	mux.HandleFunc("/loader/deploy", o.auth(o.handleDeploy))
	mux.HandleFunc("/loader/deploy/", o.auth(o.handleDeployStatus))
	mux.HandleFunc("/loader/rollback", o.auth(o.handleRollback))
	mux.HandleFunc("/loader/restart", o.auth(o.handleRestart))
	mux.HandleFunc("/loader/logs", o.auth(o.handleLogs))
	o.srv = &http.Server{Addr: o.cfg.OpsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	o.log.Infow("运维接口已启动", "addr", o.cfg.OpsAddr)
	return o.srv.ListenAndServe()
}

// Shutdown 优雅关停 HTTP 服务。
func (o *OpsServer) Shutdown(ctx context.Context) error {
	if o.srv == nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return o.srv.Shutdown(c)
}

func (o *OpsServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	pid, st := o.sup.ChildStatus()
	lb := buildinfo.Get()
	writeJSON(w, map[string]any{
		"loader": map[string]any{"version": lb.Version, "gitShort": lb.GitShort, "alive": true},
		"child": map[string]any{
			"pid":      pid,
			"healthy":  st.OK,
			"version":  st.Version,
			"gitShort": st.GitShort,
			"procPid":  st.PID,
		},
	})
}

func (o *OpsServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	pid, st := o.sup.ChildStatus()
	last, _ := o.dep.LastResult()
	cap := o.dep.Capability()
	o.sup.mu.Lock()
	restarts := o.sup.restarts
	crashes := len(o.sup.crashTimes)
	o.sup.mu.Unlock()
	writeJSON(w, map[string]any{
		"child": map[string]any{
			"pid":      pid,
			"healthy":  st.OK,
			"version":  st.Version,
			"gitShort": st.GitShort,
		},
		"supervisor": map[string]any{
			"restarts":        restarts,
			"recentCrashes":   crashes,
			"crashLoopMax":    o.cfg.CrashLoopMax,
			"crashLoopWindow": o.cfg.CrashLoopWindow.String(),
		},
		"deployCapability": cap,
		"lastDeploy":       last,
	})
}

func (o *OpsServer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Ref   string `json:"ref"`
		Force bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	res, err := o.dep.Deploy(r.Context(), body.Ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"id": res.ID, "status": res.Status})
}

func (o *OpsServer) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/loader/deploy/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	res, ok := o.dep.Result(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, res)
}

func (o *OpsServer) handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := o.dep.RestorePrev(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"status": "rolled_back"})
}

func (o *OpsServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	o.sup.Restart()
	writeJSON(w, map[string]any{"status": "restarting"})
}

// handleLogs 读取/流式 tail 子进程日志。
//   - file=console(默认)|json；lines=N(默认200)；follow=true(SSE 流式)
func (o *OpsServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	file := q.Get("file")
	if file == "" {
		file = "console"
	}
	lines := 200
	if n, err := strconv.Atoi(q.Get("lines")); err == nil && n > 0 {
		lines = n
	}
	follow := q.Get("follow") == "true" || q.Get("follow") == "1"

	name := "thinkbot.console.log"
	if file == "json" {
		name = "thinkbot.log"
	}
	path := filepath.Join(o.cfg.RuntimeDir, "logs", name)

	if follow {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, "log not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		_, _ = f.Seek(0, 2) // 从末尾开始
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				buf := make([]byte, 4096)
				n, _ := f.Read(buf)
				if n > 0 {
					fmt.Fprintf(w, "data: %s\n\n", string(buf[:n]))
					flusher.Flush()
				}
			}
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(tailLines(data, lines))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// tailLines 返回 data 的最后 n 行（按 \n 切分）。
func tailLines(data []byte, n int) []byte {
	idxs := make([]int, 0, n)
	for i, b := range data {
		if b == '\n' {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) <= n {
		return data
	}
	start := idxs[len(idxs)-n-1] + 1
	return data[start:]
}
