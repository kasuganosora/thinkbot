package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func memoryAPIServer(t *testing.T, seed []dao.TieredMemoryModel) (*Server, *gorm.DB) {
	t.Helper()
	db := newSessionTestDB(t)
	if len(seed) > 0 {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	logger := zap.NewNop().Sugar()
	svc := &BotService{dreamingBundles: map[string]*bot.DreamingBundle{}, logger: logger}
	for _, id := range []string{"A", "B"} {
		store := memory.NewTieredStoreForBot(nil, db, id)
		mgr := memory.NewTieredManager(memory.TieredManagerConfig{Store: store}, noop.NewTracerProvider(), logger)
		svc.dreamingBundles[id] = &bot.DreamingBundle{TieredMgr: mgr}
	}
	return &Server{botSvc: svc, db: db, logger: logger}, db
}

func memoryAPICall(t *testing.T, handler gin.HandlerFunc, botID, method, path, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: botID}}
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	if w.Code != http.StatusOK {
		t.Fatalf("%s response=%d %s", path, w.Code, w.Body.String())
	}
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Data
}

func memoryAPIRow(id, owner, scope, content string) dao.TieredMemoryModel {
	metadata := "{}"
	if owner != "" {
		metadata = fmt.Sprintf(`{"bot_id":%q}`, owner)
	}
	return dao.TieredMemoryModel{ID: id, Tier: int(memory.Tier1LongTerm), ScopeKind: "user", ScopeID: scope, Content: content, MetadataJSON: metadata, CreatedAt: time.Now(), Importance: .5}
}

func TestMemoryCleanupAPIsOnlyScanAndDeleteTargetBot(t *testing.T) {
	for _, kind := range []string{"trivial", "duplicates"} {
		t.Run(kind, func(t *testing.T) {
			content := "hi"
			if kind == "duplicates" {
				content = "Repeated archive reminder with useful details"
			}
			seed := []dao.TieredMemoryModel{
				memoryAPIRow("A-1", "A", "same-user", content), memoryAPIRow("A-2", "A", "same-user", content),
				memoryAPIRow("B-1", "B", "same-user", content), memoryAPIRow("B-2", "B", "same-user", content),
				memoryAPIRow("legacy", "", "same-user", content),
			}
			s, db := memoryAPIServer(t, seed)
			handler := s.handleCleanupTrivialMemory
			wantDeleted := 2
			if kind == "duplicates" {
				handler = s.handleCleanupDuplicateMemory
				wantDeleted = 1
			}
			preview := memoryAPICall(t, handler, "A", "POST", "/memory/cleanup", `{"tiers":["L1"],"minCount":2,"dryRun":true}`)
			if preview["scanned"] != float64(2) {
				t.Fatalf("cross-bot scan: %+v", preview)
			}
			encoded, _ := json.Marshal(preview)
			if bytes.Contains(encoded, []byte("B-")) || bytes.Contains(encoded, []byte("legacy")) {
				t.Fatalf("cross-bot preview: %s", encoded)
			}
			result := memoryAPICall(t, handler, "A", "POST", "/memory/cleanup", `{"tiers":["L1"],"minCount":2,"dryRun":false}`)
			if result["deleted"] != float64(wantDeleted) {
				t.Fatalf("incorrect deletion report: %+v", result)
			}
			var count int64
			db.Model(&dao.TieredMemoryModel{}).Where("id IN ?", []string{"B-1", "B-2", "legacy"}).Count(&count)
			if count != 3 {
				t.Fatalf("foreign/legacy rows deleted: remaining=%d", count)
			}
			// A repeated call must see the persisted result, without resurrecting cache.
			again := memoryAPICall(t, handler, "A", "POST", "/memory/cleanup", `{"tiers":["L1"],"minCount":2,"dryRun":false}`)
			if again["deleted"] != float64(0) {
				t.Fatalf("cleanup not idempotent: %+v", again)
			}
		})
	}
}

func TestMemoryQueryAPIHonorsSourceScopeWithinBot(t *testing.T) {
	s, _ := memoryAPIServer(t, []dao.TieredMemoryModel{
		memoryAPIRow("own-selected", "A", "u1", "selected source memory"),
		memoryAPIRow("own-other", "A", "u2", "other source memory"),
		memoryAPIRow("foreign-selected", "B", "u1", "foreign source memory"),
		memoryAPIRow("legacy", "", "u1", "unowned source memory"),
	})
	for _, query := range []string{"?tier=L1&scope=user:u1", "?scope=user:u1"} {
		data := memoryAPICall(t, s.handleQueryMemory, "A", "GET", "/memory"+query, "")
		entries := data["entries"].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["id"] != "own-selected" {
			t.Fatalf("scope filter=%+v", data)
		}
	}
	data := memoryAPICall(t, s.handleQueryMemory, "A", "GET", "/memory?tier=L1&scope=bot:B", "")
	if data["total"] != float64(0) {
		t.Fatalf("foreign bot query=%+v", data)
	}
}

func memoryImportArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := map[string]string{
		"PROFILES.md":          "## Luna\n- Name: Luna\n- Channel: web\n- Target: shared-user\n- Notes: Maintains the archive documentation carefully.\n",
		"memory/2026-04-26.md": "# Memory 2026-04-26\n\n## Entry shared-source\n\n```yaml\nid: shared-source\ncreated_at: \"2026-04-26T08:23:18Z\"\nmetadata:\n    profile_ref: Luna\n    topic: Notes\n```\n\nThe user maintains archive documentation and regularly checks the deployment instructions.\n",
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMemoryImportAPINamespacesSameArchiveByTargetBot(t *testing.T) {
	s, db := memoryAPIServer(t, nil)
	archive := memoryImportArchive(t)
	for _, id := range []string{"A", "B", "A", "B"} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", "archive.tar")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(archive); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("POST", "/memory/import", &body)
		c.Request.Header.Set("Content-Type", mw.FormDataContentType())
		s.handleImportMemohMemory(c)
		if w.Code != http.StatusOK {
			t.Fatalf("import %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	var rows []dao.TieredMemoryModel
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("same archive should yield one note/profile per bot, got %+v", rows)
	}
	counts := map[string]int{}
	for _, row := range rows {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(row.MetadataJSON), &metadata); err != nil {
			t.Fatal(err)
		}
		owner, _ := metadata["bot_id"].(string)
		counts[owner]++
		if row.ScopeKind != "user" {
			t.Fatalf("import lost user classification: %+v", row)
		}
	}
	if counts["A"] != 2 || counts["B"] != 2 {
		t.Fatalf("import ownership=%v", counts)
	}
	// A mismatched target cannot reassign entries via the lower-level import API.
	bundle, _ := s.botSvc.GetDreamingBundle("A")
	if _, err := memory.ImportMemohArchive(context.Background(), bundle.TieredMgr.Store(), "B", bytes.NewReader(archive)); err == nil {
		t.Fatal("mismatched import target accepted")
	}
}
