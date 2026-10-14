package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
)

func TestFileListHTTPDistinguishesEmptyMissingAndFailedDirectories(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	store := config.NewStore(nil)
	if err := store.Set(context.Background(), config.KeyWorkspaceDir, root); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, logger: zap.NewNop().Sugar()}
	if err := os.MkdirAll(filepath.Join(root, "bot", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bot", "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/bots/:id/files", s.handleListBotFiles)
	router.GET("/sessions/:sid/files", s.handleSessionFiles)
	for _, route := range []string{"/bots/bot/files", "/sessions/bot/files"} {
		for _, test := range []struct {
			path   string
			status int
		}{{"/empty", 200}, {"/missing", 404}, {"/file", 400}} {
			t.Run(route+test.path, func(t *testing.T) {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest("GET", route+"?path="+test.path, nil))
				if w.Code != test.status {
					t.Fatalf("got %d, want %d: %s", w.Code, test.status, w.Body.String())
				}
				if w.Code == 200 {
					var body struct {
						Data json.RawMessage `json:"data"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if string(body.Data) != "[]" && string(body.Data) != `{"entries":[],"path":"/empty"}` {
						t.Fatalf("expected explicit empty list: %s", body.Data)
					}
				}
			})
		}
	}
	if errs.GetCode(fileListError(os.ErrPermission)) != 403 {
		t.Fatal("permission errors must remain actionable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.listBotFileEntries(ctx, "bot", "/empty"); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancellation lost: %v", err)
	}
}

func TestFileListDoesNotFallbackAfterWorkspaceFailure(t *testing.T) {
	store := config.NewStore(nil)
	root := t.TempDir()
	_ = store.Set(context.Background(), config.KeyWorkspaceDir, root)
	_ = store.Set(context.Background(), config.KeySandboxBackend, "invalid-backend")
	s := &Server{store: store, logger: zap.NewNop().Sugar()}
	s.botSvc = &BotService{store: store, logger: s.logger}
	if _, err := s.listBotFileEntries(context.Background(), "bot", "/"); err == nil {
		t.Fatal("a failed workspace must not silently become an empty local directory")
	}
}
