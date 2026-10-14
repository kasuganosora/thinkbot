//go:build docker_integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

func TestTerminalHTTPExecCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"bot", "session"} {
		t.Run(kind, func(t *testing.T) {
			store := config.NewStore(nil)
			for key, value := range map[string]string{config.KeyWorkspaceDir: t.TempDir(), config.KeySandboxBackend: "docker", config.KeySandboxImage: "alpine:latest"} {
				if err := store.Set(context.Background(), key, value); err != nil {
					t.Fatal(err)
				}
			}
			s := &Server{store: store, logger: zap.NewNop().Sugar()}
			s.botSvc = &BotService{store: store, logger: s.logger}
			botID := idgen.New("terminal-test")
			mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mgr.DestroyBot(botID, true); _ = mgr.Close() })
			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{}, 2)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Next(); finished <- struct{}{} })
			path := "/bots/" + botID + "/terminal/exec"
			if kind == "bot" {
				router.POST("/bots/:id/terminal/exec", s.handleBotTerminalExec)
			} else {
				path = "/sessions/" + botID + "/terminal/exec"
				router.POST("/sessions/:sid/terminal/exec", s.handleSessionTerminalExec)
			}
			httpServer := httptest.NewServer(router)
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			requestCtx, stop := context.WithCancel(ctx)
			defer stop()
			body := `{"cmd":"(sleep 2; echo escaped > cancelled.txt) & echo ready > ready.txt; wait"}`
			req, _ := http.NewRequestWithContext(requestCtx, "POST", httpServer.URL+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			clientDone := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if resp != nil {
					data, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err == nil {
						err = fmt.Errorf("request completed: %d %s", resp.StatusCode, data)
					}
				}
				clientDone <- err
			}()
			for {
				if _, err := ws.ReadFile(ctx, "ready.txt"); err == nil {
					break
				}
				select {
				case err := <-clientDone:
					t.Fatalf("HTTP command ended before ready: %v", err)
				case <-ctx.Done():
					t.Fatal("HTTP command never started")
				case <-time.After(20 * time.Millisecond):
				}
			}
			stop() // Actual HTTP disconnect must cancel the container invocation.
			select {
			case err := <-clientDone:
				if err == nil {
					t.Fatal("expected cancelled HTTP request")
				}
			case <-ctx.Done():
				t.Fatal("HTTP client did not cancel")
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("HTTP handler did not terminate its exec")
			}
			res, err := ws.Exec(ctx, sandbox.ExecRequest{Command: "sleep 2.2; test ! -e cancelled.txt"})
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("cancelled HTTP command kept running: %+v %v", res, err)
			}

			// The non-cancelled response carries real execution outcome fields on
			// both routes, rather than translating a nonzero exit into success.
			req, _ = http.NewRequestWithContext(ctx, "POST", httpServer.URL+path, strings.NewReader(`{"cmd":"echo output; exit 7"}`))
			req.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			var result struct {
				Data struct {
					ExitCode int  `json:"exitCode"`
					Reliable bool `json:"reliable"`
					Aborted  bool `json:"aborted"`
				} `json:"data"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || result.Data.ExitCode != 7 || !result.Data.Reliable || result.Data.Aborted {
				t.Fatalf("execution result lost: %s", data)
			}
		})
	}
}
