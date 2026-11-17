package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/dao"
)

func TestModelTemperatureSentinelRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, st := applyDB(t)
	srv := &Server{store: st, logger: zap.NewNop().Sugar()}
	r := gin.New()
	r.POST("/api/providers", srv.handleCreateProvider)
	r.POST("/api/providers/:pid/models", srv.handleAddModel)
	r.PUT("/api/providers/:pid/models/:mid", srv.handleUpdateModel)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	tempOf := func(w *httptest.ResponseRecorder) float64 {
		t.Helper()
		var resp struct {
			Data struct {
				Temperature float64 `json:"temperature"`
				TopP        float64 `json:"topP"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json %v body %s", err, w.Body.String())
		}
		if resp.Data.TopP != -1 && w.Code == http.StatusOK && resp.Data.Temperature == -1 {
			t.Fatalf("topP not stored: %s", w.Body.String())
		}
		return resp.Data.Temperature
	}

	w := call(http.MethodPost, "/api/providers", `{"id":"provider-synth-a","name":"synth","clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w = call(http.MethodPost, "/api/providers/provider-synth-a/models", `{"id":"model-sol-main","name":"m","temperature":-1,"topP":-1}`)
	if w.Code != http.StatusOK || tempOf(w) != -1 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	def, err := srv.getProvider("provider-synth-a")
	if err != nil || len(def.Models) != 1 || def.Models[0].Temperature != -1 || def.Models[0].TopP != -1 {
		t.Fatalf("stored %+v err %v", def, err)
	}
	for _, bad := range []string{`{"temperature":-0.5}`, `{"temperature":-2}`, `{"topP":-0.5}`, `{"topP":-2}`} {
		w = call(http.MethodPut, "/api/providers/provider-synth-a/models/model-sol-main", bad)
		if w.Code == http.StatusOK {
			t.Fatalf("accepted %s: %s", bad, w.Body.String())
		}
	}
	def, _ = srv.getProvider("provider-synth-a")
	if def.Models[0].Temperature != -1 || def.Models[0].TopP != -1 {
		t.Fatalf("rejected update changed stored values: %+v", def.Models[0])
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a/models/model-sol-main", `{"temperature":0.4}`)
	if w.Code != http.StatusOK {
		t.Fatalf("explicit update %d %s", w.Code, w.Body.String())
	}
	def, _ = srv.getProvider("provider-synth-a")
	if def.Models[0].Temperature != 0.4 || def.Models[0].TopP != -1 {
		t.Fatalf("after explicit update %+v", def.Models[0])
	}
}

func TestBotTemperatureSentinelRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, st := applyDB(t)
	svc := NewBotService(db, st, bot.NewBotManager(zap.NewNop().Sugar(), nooptrace.NewTracerProvider()), zap.NewNop().Sugar(), nooptrace.NewTracerProvider(), nil, nil, nil, nil, nil, nil, nil, nil)
	srv := &Server{store: st, botSvc: svc, logger: zap.NewNop().Sugar()}
	r := gin.New()
	r.POST("/api/bots", srv.handleCreateBot)
	r.GET("/api/bots", srv.handleListBots)
	r.GET("/api/bots/:id", srv.handleGetBot)
	r.PUT("/api/bots/:id", srv.handleUpdateBot)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		var reader *bytes.Reader
		if body != "" {
			reader = bytes.NewReader([]byte(body))
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		r.ServeHTTP(w, req)
		return w
	}

	readBotTemp := func(w *httptest.ResponseRecorder) (string, float64) {
		t.Helper()
		var resp struct {
			Data struct {
				ID          string  `json:"id"`
				Temperature float64 `json:"temperature"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json %v body %s", err, w.Body.String())
		}
		return resp.Data.ID, resp.Data.Temperature
	}

	// Create with explicit -1 (omit)
	w := call(http.MethodPost, "/api/bots", `{"name":"bot-omit","temperature":-1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create -1 failed: %d %s", w.Code, w.Body.String())
	}
	botOmitID, gotTemp := readBotTemp(w)
	if gotTemp != -1 {
		t.Fatalf("create -1 echoed temperature=%v, want -1", gotTemp)
	}

	// Reload via GET /api/bots/:id
	w = call(http.MethodGet, "/api/bots/"+botOmitID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get -1 failed: %d %s", w.Code, w.Body.String())
	}
	if _, gotTemp = readBotTemp(w); gotTemp != -1 {
		t.Fatalf("reload -1 temperature=%v, want -1", gotTemp)
	}

	// Create with explicit 0 (unset, not snapped to 0.7 on insert)
	w = call(http.MethodPost, "/api/bots", `{"name":"bot-zero","temperature":0}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create 0 failed: %d %s", w.Code, w.Body.String())
	}
	botZeroID, gotTemp := readBotTemp(w)
	if gotTemp != 0 {
		t.Fatalf("create 0 echoed temperature=%v, want 0", gotTemp)
	}
	w = call(http.MethodGet, "/api/bots/"+botZeroID, "")
	if _, gotTemp = readBotTemp(w); gotTemp != 0 {
		t.Fatalf("reload 0 temperature=%v, want 0", gotTemp)
	}

	// Invalid negatives are rejected on create and update without mutating stored value
	for _, bad := range []string{`{"name":"bad","temperature":-0.5}`, `{"name":"bad","temperature":-2}`} {
		if badW := call(http.MethodPost, "/api/bots", bad); badW.Code == http.StatusOK {
			t.Fatalf("accepted invalid create %s: %s", bad, badW.Body.String())
		}
	}
	for _, bad := range []string{`{"temperature":-0.5}`, `{"temperature":-2}`} {
		if badW := call(http.MethodPut, "/api/bots/"+botOmitID, bad); badW.Code == http.StatusOK {
			t.Fatalf("accepted invalid update %s: %s", bad, badW.Body.String())
		}
	}
	w = call(http.MethodGet, "/api/bots/"+botOmitID, "")
	if _, gotTemp = readBotTemp(w); gotTemp != -1 {
		t.Fatalf("rejected update mutated stored temperature=%v, want -1", gotTemp)
	}

	// Update -1 -> 0 -> -1 and confirm echo + list stability
	for _, want := range []float64{0, -1, 0.8, -1} {
		w = call(http.MethodPut, "/api/bots/"+botOmitID, `{"temperature":`+jsonNumber(want)+`}`)
		if w.Code != http.StatusOK {
			t.Fatalf("update %v failed: %d %s", want, w.Code, w.Body.String())
		}
		w = call(http.MethodGet, "/api/bots/"+botOmitID, "")
		if _, gotTemp = readBotTemp(w); gotTemp != want {
			t.Fatalf("after update %v, reload got %v", want, gotTemp)
		}
	}
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestBotCreateAtomicSingleWriteDistinguishesOmittedZeroAndMinusOne(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, st := applyDB(t)

	var createWrites atomic.Int32
	var updateWrites atomic.Int32
	var blockUpdates atomic.Bool

	if err := db.Callback().Create().Before("gorm:create").Register("test:count_bot_creates", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == (dao.BotDefinition{}).TableName() {
			createWrites.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("test:block_bot_updates", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == (dao.BotDefinition{}).TableName() {
			updateWrites.Add(1)
			if blockUpdates.Load() {
				_ = tx.AddError(errors.New("unexpected follow-up UPDATE during bot creation"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewBotService(db, st, bot.NewBotManager(zap.NewNop().Sugar(), nooptrace.NewTracerProvider()), zap.NewNop().Sugar(), nooptrace.NewTracerProvider(), nil, nil, nil, nil, nil, nil, nil, nil)
	srv := &Server{store: st, botSvc: svc, logger: zap.NewNop().Sugar()}
	r := gin.New()
	r.POST("/api/bots", srv.handleCreateBot)
	r.GET("/api/bots/:id", srv.handleGetBot)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		var reader *bytes.Reader
		if body != "" {
			reader = bytes.NewReader([]byte(body))
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		r.ServeHTTP(w, req)
		return w
	}

	readBot := func(w *httptest.ResponseRecorder) dao.BotDefinition {
		t.Helper()
		var resp struct {
			Data dao.BotDefinition `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json %v body %s", err, w.Body.String())
		}
		return resp.Data
	}

	// Block all UPDATE statements during POST /api/bots to enforce single-write atomicity.
	blockUpdates.Store(true)

	// 1. Omitted temperature -> default 0.7 in a single INSERT.
	w := call(http.MethodPost, "/api/bots", `{"name":"bot-omitted-temp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create omitted failed: %d %s", w.Code, w.Body.String())
	}
	omittedBot := readBot(w)
	if omittedBot.Temperature != 0.7 {
		t.Fatalf("omitted create echoed temperature=%v, want 0.7", omittedBot.Temperature)
	}
	if createWrites.Load() != 1 || updateWrites.Load() != 0 {
		t.Fatalf("omitted create writes: creates=%d updates=%d, want 1 and 0", createWrites.Load(), updateWrites.Load())
	}
	if got := readBot(call(http.MethodGet, "/api/bots/"+omittedBot.ID, "")); got.Temperature != 0.7 {
		t.Fatalf("omitted reload temperature=%v, want 0.7", got.Temperature)
	}

	// 2. Explicit 0 -> persists 0 in a single INSERT (no follow-up UPDATE, no orphan 0.7 row).
	w = call(http.MethodPost, "/api/bots", `{"name":"bot-explicit-zero","temperature":0}`)
	if w.Code != http.StatusOK {
		var rows []dao.BotDefinition
		_ = db.Where("name = ?", "bot-explicit-zero").Find(&rows).Error
		t.Fatalf("create explicit 0 failed (status=%d, updates=%d, left_rows=%+v): %s", w.Code, updateWrites.Load(), rows, w.Body.String())
	}
	zeroBot := readBot(w)
	if zeroBot.Temperature != 0 {
		t.Fatalf("explicit 0 create echoed temperature=%v, want 0", zeroBot.Temperature)
	}
	if createWrites.Load() != 2 || updateWrites.Load() != 0 {
		t.Fatalf("explicit 0 create writes: creates=%d updates=%d, want 2 and 0", createWrites.Load(), updateWrites.Load())
	}
	var storedZero dao.BotDefinition
	if err := db.First(&storedZero, "id = ?", zeroBot.ID).Error; err != nil {
		t.Fatalf("load stored explicit 0: %v", err)
	}
	if storedZero.Temperature != 0 {
		t.Fatalf("stored explicit 0 temperature=%v, want 0", storedZero.Temperature)
	}
	if storedZero.MemoryLimitMB != 2048 || storedZero.MaxTokens != 4096 || storedZero.Workers != 4 {
		t.Fatalf("other GORM defaults lost on explicit 0 create: %+v", storedZero)
	}

	// 3. Explicit -1 -> persists -1 in a single INSERT.
	w = call(http.MethodPost, "/api/bots", `{"name":"bot-explicit-omit","temperature":-1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create explicit -1 failed: %d %s", w.Code, w.Body.String())
	}
	minusOneBot := readBot(w)
	if minusOneBot.Temperature != -1 {
		t.Fatalf("explicit -1 create echoed temperature=%v, want -1", minusOneBot.Temperature)
	}
	if createWrites.Load() != 3 || updateWrites.Load() != 0 {
		t.Fatalf("explicit -1 create writes: creates=%d updates=%d, want 3 and 0", createWrites.Load(), updateWrites.Load())
	}

	// 4. Direct Go CreateDefinition with unset struct zero value keeps GORM default 0.7.
	direct := &dao.BotDefinition{ID: "bot-direct-default", Name: "direct"}
	if err := svc.CreateDefinition(direct); err != nil {
		t.Fatalf("direct CreateDefinition failed: %v", err)
	}
	var storedDirect dao.BotDefinition
	if err := db.First(&storedDirect, "id = ?", direct.ID).Error; err != nil {
		t.Fatalf("load direct bot: %v", err)
	}
	if storedDirect.Temperature != 0.7 {
		t.Fatalf("direct CreateDefinition temperature=%v, want 0.7", storedDirect.Temperature)
	}
}
