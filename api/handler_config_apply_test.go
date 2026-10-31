package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCPUConfigSaveReportsUnappliedState(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			store := config.NewStore(nil)
			s := &Server{store: store, logger: zap.NewNop().Sugar()}
			body := `{"value":"4"}`
			if batch {
				body = `{"items":{"sandbox.cpu_limit":"4"}}`
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "key", Value: config.KeySandboxCPULimit}}
			c.Request = httptest.NewRequest("PUT", "/api/config", bytes.NewBufferString(body))
			c.Request.Header.Set("Content-Type", "application/json")
			if batch {
				s.handleBatchSetConfig(c)
			} else {
				s.handleSetConfigKey(c)
			}
			if w.Code != 200 {
				t.Fatalf("save: %d %s", w.Code, w.Body.String())
			}
			var response struct {
				Data SandboxLimitApplyResult `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Data.Applied || !response.Data.Failed {
				t.Fatalf("unavailable runtime reported applied: %+v", response.Data)
			}
			if got := store.GetString(config.KeySandboxCPULimit, ""); got != "4" {
				t.Fatalf("saved value=%q", got)
			}
		})
	}
}

func TestCPUApplyReportsDefinitionLoadFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	s := &BotService{db: db, store: config.NewStore(nil), logger: zap.NewNop().Sugar()}
	result := s.ApplySandboxCPULimitAll(context.Background())
	if !result.Failed || result.Applied {
		t.Fatalf("missing schema treated as success: %+v", result)
	}
	if err := db.AutoMigrate(&dao.BotDefinition{}); err != nil {
		t.Fatal(err)
	}
	result = s.ApplySandboxCPULimitAll(context.Background())
	if result.Failed || result.NeedsRecreate || !result.Applied {
		t.Fatalf("empty fleet: %+v", result)
	}
}
