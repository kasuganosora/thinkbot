package api

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestSessionStatusSeparatesCumulativeUsageFromUnavailableContext(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "status.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&dao.UsageDaily{}); err != nil {
		t.Fatal(err)
	}
	rows := []dao.UsageDaily{
		{BotID: "selected", Model: "main", Feature: "chat", Date: time.Now(), TotalTokens: 1500, TotalRequests: 2, CacheHitRequests: 1},
		{BotID: "selected", Model: "light", Feature: "summary", Date: time.Now(), TotalTokens: 300, TotalRequests: 1},
		{BotID: "other", Model: "main", Feature: "chat", Date: time.Now(), TotalTokens: 9000},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, logger: zap.NewNop().Sugar()}
	status := s.getSessionStatus("selected")
	if status.Scope != "bot" || status.BotID != "selected" || status.TotalTokens != 1800 || status.ContextUsed != status.TotalTokens {
		t.Fatalf("cumulative bot accounting changed or leaked: %+v", status)
	}
	if status.Context.Status != "unavailable" || status.Context.Reason == "" || status.Context.Used != nil || status.Context.Limit != nil || status.ContextLimit != nil {
		t.Fatalf("an unavailable window must not be a fake zero/unlimited window: %+v", status)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	context := wire["context"].(map[string]any)
	for _, field := range []string{"used", "limit"} {
		if value, present := context[field]; !present || value != nil {
			t.Fatalf("context.%s must be explicitly null: %s", field, data)
		}
	}
}

func TestSessionStatusWithoutStatsStillReportsUnavailableContext(t *testing.T) {
	status := (&Server{logger: zap.NewNop().Sugar()}).getSessionStatus("bot")
	if status.Context.Status != "unavailable" || status.TotalTokens != 0 || status.Context.Used != nil {
		t.Fatalf("unexpected status: %+v", status)
	}
}
