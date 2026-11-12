package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/channel/telegram"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func applyDB(t *testing.T) (*gorm.DB, *config.Store) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.Setting{}, &dao.BotDefinition{}); err != nil {
		t.Fatal(err)
	}
	st := config.NewStore(db)
	if err := st.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, st
}

func putProvider(t *testing.T, st *config.Store, id, body string) {
	t.Helper()
	if err := st.Set(context.Background(), "provider."+id, body); err != nil {
		t.Fatal(err)
	}
}

func TestStartBot_InvalidModelKeepsRunningBot(t *testing.T) {
	db, st := applyDB(t)
	keep := &bot.Bot{}
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{"bot-synth": keep}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-missing", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-sol-main"); err != nil {
		t.Fatal(err)
	}
	err := svc.StartBot(context.Background(), "bot-synth")
	if err == nil {
		t.Fatal("expected start to fail")
	}
	if svc.botInstances["bot-synth"] != keep {
		t.Fatal("preparation failure replaced the running bot")
	}
	got, _ := st.Get(config.BotLLMKey("bot-synth", "main"))
	if got != "model-sol-main" {
		t.Fatalf("effective key changed to %s", got)
	}
}

func TestStartBot_InvalidModelDoesNotOverwriteEffective(t *testing.T) {
	db, st := applyDB(t)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-missing", Status: dao.BotStatusStopped}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-sol-main"); err != nil {
		t.Fatal(err)
	}
	err := svc.StartBot(context.Background(), "bot-synth")
	if err == nil {
		t.Fatal("expected start to fail")
	}
	got, _ := st.Get(config.BotLLMKey("bot-synth", "main"))
	if got != "model-sol-main" {
		t.Fatalf("effective key changed to %s", got)
	}
}

func TestApplyDesired_SwapsRunningSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"},{"id":"model-terra-light"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", LLMLight: "model-terra-light"}); err != nil {
		t.Fatal(err)
	}
	old := &bot.LLMSnapshot{MainModel: llm.ChatModel("model-old")}
	slot := bot.NewLLMSlot(old)
	svc.llmSlots["bot-synth"] = slot
	svc.botInstances["bot-synth"] = &bot.Bot{}

	res, err := svc.ApplyDesiredLLM(context.Background(), "bot-synth")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || slot.Current().MainModel.ID != "model-sol-main" || slot.Current().LightModel.ID != "model-terra-light" {
		t.Fatalf("apply %+v snap main %v", res, slot.Current())
	}
	got, _ := st.Get(config.BotLLMKey("bot-synth", "main"))
	if got != "model-sol-main" {
		t.Fatalf("effective %s", got)
	}
}

func TestApplyDesired_FailureKeepsPreviousSnapshot(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-missing"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-sol-main"); err != nil {
		t.Fatal(err)
	}
	slot := bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-sol-main")})
	svc.llmSlots["bot-synth"] = slot
	svc.botInstances["bot-synth"] = &bot.Bot{}
	res, err := svc.ApplyDesiredLLM(context.Background(), "bot-synth")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || slot.Current().MainModel.ID != "model-sol-main" {
		t.Fatalf("rolled forward: %+v", res)
	}
	got, _ := st.Get(config.BotLLMKey("bot-synth", "main"))
	if got != "model-sol-main" {
		t.Fatalf("effective changed to %s", got)
	}
}

func TestProviderCredentialAndProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, st := applyDB(t)
	logger := zap.NewNop().Sugar()
	srv := &Server{store: st, logger: logger}
	var hits atomic.Int32
	var authOK atomic.Bool
	const token = "dummy-token-alpha-aaaa"
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1 {
			authOK.Store(true)
		}
		if r.URL.Path == "/hang" {
			time.Sleep(2 * time.Second)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"model-sol-main"}]}`)
	}))
	defer stub.Close()

	r := gin.New()
	r.POST("/api/providers", srv.handleCreateProvider)
	r.PUT("/api/providers/:pid", srv.handleUpdateProvider)
	r.POST("/api/providers/:pid/test", srv.handleTestProvider)
	r.PUT("/api/config/:key", srv.handleSetConfigKey)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	w := call(http.MethodPost, "/api/providers", `{"id":"provider-synth-a","name":"synth","clientType":"OpenAI Compatible","baseUrl":"`+stub.URL+`/v1"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	def, err := srv.getProvider("provider-synth-a")
	if err != nil || def.APIKey != "" {
		t.Fatalf("create key %v %v", def, err)
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"apiKey":"`+token+`","enabled":true}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	before := hits.Load()
	w = call(http.MethodPost, "/api/providers/provider-synth-a/test", "")
	if hits.Load() != before+1 || !authOK.Load() {
		t.Fatalf("probe hits %d auth %v body %s", hits.Load()-before, authOK.Load(), w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_list_ok") || strings.Contains(w.Body.String(), token) {
		t.Fatalf("probe body %s", w.Body.String())
	}
	masked := maskAPIKey(token)
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"apiKey":"`+masked+`"}`)
	if w.Code == 200 {
		t.Fatalf("mask was accepted: %s", w.Body.String())
	}
	def, _ = srv.getProvider("provider-synth-a")
	if def.APIKey != token {
		t.Fatal("mask overwrite changed the stored key")
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"name":"renamed"}`)
	def, _ = srv.getProvider("provider-synth-a")
	if def.APIKey != token || def.Name != "renamed" {
		t.Fatal("omit did not keep the key")
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"apiKey":""}`)
	def, _ = srv.getProvider("provider-synth-a")
	if def.APIKey != "" {
		t.Fatal("empty did not clear")
	}
	w = call(http.MethodPut, "/api/config/provider.provider-synth-a", `{"value":"{}"}`)
	if w.Code == 200 {
		t.Fatalf("generic config wrote a provider: %s", w.Body.String())
	}
	_ = json.Valid(w.Body.Bytes())
}

func TestProbeTimeoutAndRedirect(t *testing.T) {
	var otherHits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer final.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/models", http.StatusFound)
	}))
	defer redirect.Close()
	def := &ProviderDef{ClientType: "OpenAI Compatible", BaseURL: redirect.URL, APIKey: "dummy-token-alpha-aaaa", Enabled: true}
	client := &http.Client{Timeout: time.Second}
	got := probeProvider(context.Background(), def, client)
	if got.OK || got.Kind != "redirect_blocked" || otherHits.Load() != 0 {
		t.Fatalf("cross-origin redirect probe %+v hits %d", got, otherHits.Load())
	}

	var sameHits atomic.Int32
	var sameAuth atomic.Bool
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/v1/model-list", http.StatusFound)
			return
		}
		sameHits.Add(1)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer dummy-token-alpha-aaaa")) == 1 {
			sameAuth.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer same.Close()
	got = probeProvider(context.Background(), &ProviderDef{ClientType: "OpenAI Compatible", BaseURL: same.URL + "/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: true}, &http.Client{Timeout: time.Second})
	if !got.OK || !sameAuth.Load() || sameHits.Load() != 1 {
		t.Fatalf("same-origin redirect %+v hits %d auth %v", got, sameHits.Load(), sameAuth.Load())
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer hang.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got = probeProvider(ctx, &ProviderDef{ClientType: "OpenAI Compatible", BaseURL: hang.URL + "/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: false}, &http.Client{Timeout: 50 * time.Millisecond})
	if got.Kind != "network" || got.OK {
		t.Fatalf("timeout probe %+v", got)
	}
}

func TestProbe_InjectedRedirectCannotLeaveOrigin(t *testing.T) {
	var otherHits atomic.Int32
	var sawAuth atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/models", http.StatusFound)
	}))
	defer origin.Close()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return nil
	}}
	got := probeProvider(context.Background(), &ProviderDef{
		ClientType: "OpenAI Compatible", BaseURL: origin.URL + "/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: true,
	}, client)
	if got.OK || got.Kind != "redirect_blocked" || otherHits.Load() != 0 || sawAuth.Load() {
		t.Fatalf("injected redirect %+v hits %d auth %v", got, otherHits.Load(), sawAuth.Load())
	}
	httpsURL, _ := url.Parse("https://models.example/v1/models")
	httpURL, _ := url.Parse("http://models.example/v1/models")
	req, _ := http.NewRequest(http.MethodGet, httpURL.String(), nil)
	via, _ := http.NewRequest(http.MethodGet, httpsURL.String(), nil)
	if err := stripCrossHostAuth(req, []*http.Request{via}); err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("downgrade err %v", err)
	}
}

func TestProbe_BodyCapAndDefaultTimeout(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	capped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), probeBodyCap))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, reflected)
		time.Sleep(3 * time.Second)
	}))
	defer capped.Close()
	start := time.Now()
	got := probeProvider(context.Background(), &ProviderDef{ClientType: "OpenAI Compatible", BaseURL: capped.URL + "/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: true}, &http.Client{Timeout: 5 * time.Second})
	if !got.OK || strings.Contains(got.Message, reflected) || time.Since(start) > 2*time.Second {
		t.Fatalf("capped probe elapsed %s %+v", time.Since(start), got)
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer hang.Close()
	start = time.Now()
	got = probeProvider(context.Background(), &ProviderDef{ClientType: "OpenAI Compatible", BaseURL: hang.URL + "/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: true}, nil)
	elapsed := time.Since(start)
	if got.Kind != "network" || elapsed < 8*time.Second || elapsed > 15*time.Second {
		t.Fatalf("default timeout elapsed %s %+v", elapsed, got)
	}
}

func TestRequestOrigin_SchemeAndPort(t *testing.T) {
	a, _ := urlParse("https://models.example/v1")
	b, _ := urlParse("http://models.example/v1")
	c, _ := urlParse("https://models.example:8443/v1")
	d, _ := urlParse("https://models.example:443/v1")
	if requestOrigin(a) == requestOrigin(b) {
		t.Fatal("https and http must differ")
	}
	if requestOrigin(a) == requestOrigin(c) {
		t.Fatal("port must differ")
	}
	if requestOrigin(a) != requestOrigin(d) {
		t.Fatalf("default https port mismatch %s vs %s", requestOrigin(a), requestOrigin(d))
	}
}

func urlParse(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

func TestChooseStartupModels(t *testing.T) {
	ok := map[string]bool{"model-sol-main": true, "model-terra-light": true}
	resolve := func(id string) error {
		if ok[id] {
			return nil
		}
		return fmt.Errorf("model %q is not configured on an enabled provider", id)
	}
	mainID, lightID, fromDesired, err := chooseStartupModels("model-missing", "model-terra-light", "model-sol-main", "model-terra-light", resolve)
	if fromDesired || err == nil || mainID != "model-sol-main" || lightID != "model-terra-light" {
		t.Fatalf("fallback %s %s desired %v err %v", mainID, lightID, fromDesired, err)
	}
	_, _, _, err = chooseStartupModels("", "", "model-sol-main", "", resolve)
	if err == nil {
		t.Fatal("empty desired must not fall back")
	}
	mainID, _, fromDesired, err = chooseStartupModels("model-sol-main", "", "", "", resolve)
	if err != nil || !fromDesired || mainID != "model-sol-main" {
		t.Fatalf("desired %s %v %v", mainID, fromDesired, err)
	}
}

func TestReapply_OneFailurePublishesNone(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-a", Name: "a", LLMMain: "model-sol-main"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-b", Name: "b", LLMMain: "model-missing"}); err != nil {
		t.Fatal(err)
	}
	slotA := bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-sol-main"), MainDef: config.ModelDef{ProviderID: "provider-synth-a", Model: "model-sol-main"}})
	slotB := bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-old"), MainDef: config.ModelDef{ProviderID: "provider-synth-a", Model: "model-old"}})
	svc.llmSlots["bot-a"] = slotA
	svc.llmSlots["bot-b"] = slotB
	ok, msg := svc.reapplyRunningBots(context.Background(), "provider-synth-a")
	if ok || msg == "" {
		t.Fatalf("expected failure, ok %v msg %q", ok, msg)
	}
	if slotA.Current().MainModel.ID != "model-sol-main" || slotB.Current().MainModel.ID != "model-old" {
		t.Fatalf("snapshots changed %s %s", slotA.Current().MainModel.ID, slotB.Current().MainModel.ID)
	}
}

func TestReapply_SecondPersistFailureWritesNone(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"},{"id":"model-terra-light"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-a", Name: "a", LLMMain: "model-sol-main"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-b", Name: "b", LLMMain: "model-terra-light"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-a", "main"), "model-old"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-b", "main"), "model-old"); err != nil {
		t.Fatal(err)
	}
	slotA := bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-old"), MainDef: config.ModelDef{ProviderID: "provider-synth-a", Model: "model-old"}})
	slotB := bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-old"), MainDef: config.ModelDef{ProviderID: "provider-synth-a", Model: "model-old"}})
	svc.llmSlots["bot-a"] = slotA
	svc.llmSlots["bot-b"] = slotB
	var calls atomic.Int32
	testPersistEffective = func(ids []string) error {
		n := calls.Add(1)
		if n >= 2 || len(ids) > 1 {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	defer func() { testPersistEffective = nil }()
	ok, msg := svc.reapplyRunningBots(context.Background(), "provider-synth-a")
	if ok || msg == "" {
		t.Fatalf("expected persist failure, ok %v msg %q", ok, msg)
	}
	for _, id := range []string{"bot-a", "bot-b"} {
		got, _ := st.Get(config.BotLLMKey(id, "main"))
		if got != "model-old" {
			t.Fatalf("%s effective changed to %s", id, got)
		}
	}
	if slotA.Current().MainModel.ID != "model-old" || slotB.Current().MainModel.ID != "model-old" {
		t.Fatalf("snapshots changed %s %s", slotA.Current().MainModel.ID, slotB.Current().MainModel.ID)
	}
}

func TestApply_OverlappingSavesStayPaired(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"},{"id":"model-terra-light"},{"id":"model-old"},{"id":"model-other"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{"bot-synth": {}}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-old", LLMLight: "model-other"}); err != nil {
		t.Fatal(err)
	}
	svc.llmSlots["bot-synth"] = bot.NewLLMSlot(&bot.LLMSnapshot{MainModel: llm.ChatModel("model-old"), LightModel: llm.ChatModel("model-other")})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			main, light := "model-sol-main", "model-terra-light"
			if n%2 == 0 {
				main, light = "model-old", "model-other"
			}
			svc.applyMu.Lock()
			_ = svc.db.Model(&dao.BotDefinition{}).Where("id = ?", "bot-synth").Updates(map[string]any{"llm_main": main, "llm_light": light})
			_, _ = svc.applyDesiredLocked(context.Background(), "bot-synth")
			svc.applyMu.Unlock()
		}(i)
	}
	wg.Wait()
	cur := svc.llmSlots["bot-synth"].Current()
	pairA := cur.MainModel.ID == "model-sol-main" && cur.LightModel.ID == "model-terra-light"
	pairB := cur.MainModel.ID == "model-old" && cur.LightModel.ID == "model-other"
	if !pairA && !pairB {
		t.Fatalf("mixed snapshot main %s light %s", cur.MainModel.ID, cur.LightModel.ID)
	}
}

type idProvider struct{ id string }

func (p *idProvider) Name() string { return p.id }
func (p *idProvider) DoGenerate(context.Context, llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: p.id}, nil
}
func (p *idProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, fmt.Errorf("not used")
}

func TestApply_PublishFailureRestoresKeysAndClient(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-old"); err != nil {
		t.Fatal(err)
	}
	oldClient := &idProvider{id: "old-client"}
	slot := bot.NewLLMSlot(&bot.LLMSnapshot{Main: oldClient, MainModel: llm.ChatModel("model-old")})
	svc.llmSlots["bot-synth"] = slot
	svc.botInstances["bot-synth"] = &bot.Bot{}
	testAfterEffectivePublish = func(string) error { return fmt.Errorf("publish failed") }
	defer func() { testAfterEffectivePublish = nil }()
	res, err := svc.ApplyDesiredLLM(context.Background(), "bot-synth")
	if err == nil || res.Applied {
		t.Fatalf("expected publish failure, res %+v err %v", res, err)
	}
	got, _ := st.Get(config.BotLLMKey("bot-synth", "main"))
	if got != "model-old" {
		t.Fatalf("effective key %s", got)
	}
	if slot.Current().Main != oldClient {
		t.Fatal("client pointer changed")
	}
	out, err := slot.Current().Main.DoGenerate(context.Background(), llm.GenerateParams{})
	if err != nil || out.Text != "old-client" {
		t.Fatalf("usage %v %v", out, err)
	}
}

func TestCommitEffective_FailuresKeepPreviousPair(t *testing.T) {
	db, st := applyDB(t)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar()}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-old"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "light"), "model-light-old"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "vision_effective"), "model-vision-old"); err != nil {
		t.Fatal(err)
	}
	installed := false
	testPersistEffective = func([]string) error { return fmt.Errorf("disk full") }
	err := svc.commitEffectivePublication(context.Background(), "bot-synth", effectiveRow{id: "bot-synth", main: "model-sol-main", light: "model-terra-light", vision: "model-vision"}, true, func() error {
		installed = true
		return nil
	})
	testPersistEffective = nil
	if err == nil || installed {
		t.Fatalf("persist failure installed=%v err %v", installed, err)
	}
	if got, _ := st.Get(config.BotLLMKey("bot-synth", "main")); got != "model-old" {
		t.Fatalf("main key %s", got)
	}
	err = svc.commitEffectivePublication(context.Background(), "bot-synth", effectiveRow{id: "bot-synth", main: "model-sol-main", light: "model-terra-light", vision: "model-vision"}, true, func() error {
		return fmt.Errorf("publish failed")
	})
	if err == nil {
		t.Fatal("expected publish error")
	}
	if got, _ := st.Get(config.BotLLMKey("bot-synth", "main")); got != "model-old" {
		t.Fatalf("main restored to %s", got)
	}
	if got, _ := st.Get(config.BotLLMKey("bot-synth", "light")); got != "model-light-old" {
		t.Fatalf("light restored to %s", got)
	}
	if got, _ := st.Get(config.BotLLMKey("bot-synth", "vision_effective")); got != "model-vision-old" {
		t.Fatalf("vision restored to %s", got)
	}
}

func TestStartupDesiredUnchangedGuard(t *testing.T) {
	prepared := startupLLM{desiredMain: "model-sol-main", desiredLight: "model-terra-light", desiredVision: "model-vision"}
	same := &dao.BotDefinition{LLMMain: "model-sol-main", LLMLight: "model-terra-light"}
	if startupDesiredChanged(same, "model-vision", prepared) {
		t.Fatal("matching desired was treated as a change")
	}
	if !startupDesiredChanged(same, "model-other", prepared) {
		t.Fatal("vision change was ignored")
	}
}

func TestRestartAfterKeyCommitUsesDesired(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-old","temperature":0.2,"maxTokens":111},{"id":"model-sol-main","temperature":0.8,"maxTokens":222}]}`)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar()}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-synth", "main"), "model-old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.commitEffectivePublication(context.Background(), "bot-synth", effectiveRow{id: "bot-synth", main: "model-sol-main", light: "", vision: ""}, true, nil); err != nil {
		t.Fatal(err)
	}
	def, err := svc.GetDefinition("bot-synth")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := svc.prepareStartupLLM(def)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.fromDesired || prepared.mainID != "model-sol-main" {
		t.Fatalf("restart prepare %+v", prepared)
	}
	if got, _ := st.Get(config.BotLLMKey("bot-synth", "main")); got != "model-sol-main" {
		t.Fatalf("committed key %s", got)
	}
}

func TestStartBot_ReadyTimeoutResumesOldPoll(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://gen-1.example/v1","apiKey":"gen-1","models":[{"id":"model-sol-main"}]}`)
	var active atomic.Int32
	var maxSeen atomic.Int32
	var phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":7,"is_bot":true,"username":"synth"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(body, &req)
		n := active.Add(1)
		for {
			seen := maxSeen.Load()
			if n <= seen || maxSeen.CompareAndSwap(seen, n) {
				break
			}
		}
		defer active.Add(-1)
		if req.Offset == 0 && phase.Load() == 0 {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"text":"hi","chat":{"id":1,"type":"private"},"from":{"id":2}}}]}`))
			return
		}
		if req.Offset == 11 && phase.Load() == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":11,"message":{"message_id":2,"text":"next","chat":{"id":1,"type":"private"},"from":{"id":2}}}]}`))
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	rawPlatforms := `[{"id":"p","type":"telegram","name":"tg-new","enabled":true,"config":{"token":"123456:TEST","apiBaseUrl":"` + hang.URL + `","pollTimeout":1}}]`
	if err := st.Set(context.Background(), "bot.bot-synth.detail.platforms", rawPlatforms); err != nil {
		t.Fatal(err)
	}
	logger := zap.NewNop().Sugar()
	tp := nooptrace.NewTracerProvider()
	ch := telegram.NewChannel("tg-old", "bot-synth", telegram.Config{Token: "123456:TEST", APIBaseURL: srv.URL, PollTimeout: 1, StreamReply: false})
	pipe, err := pipeline.New(nil, tp, noopmetric.NewMeterProvider(), logger)
	if err != nil {
		t.Fatal(err)
	}
	old, err := bot.New(bot.BotParams{ID: "bot-synth", Logger: logger, TP: tp, Dispatcher: outbound.NewMultiDispatcher(logger, tp), Pipeline: pipe, Channels: []bot.Channel{ch}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() { _ = old.Run(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(containsInt(ch.HandledUpdateIDs(), 10) && active.Load() >= 1) {
		time.Sleep(10 * time.Millisecond)
	}
	svc := &BotService{db: db, store: st, logger: logger, tp: tp, mp: noopmetric.NewMeterProvider(), botInstances: map[string]*bot.Bot{"bot-synth": old}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	testReadyTimeout = 200 * time.Millisecond
	defer func() { testReadyTimeout = 0 }()
	phase.Store(1)
	err = svc.StartBot(context.Background(), "bot-synth")
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout path: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !containsInt(ch.HandledUpdateIDs(), 11) {
		time.Sleep(10 * time.Millisecond)
	}
	ids := ch.HandledUpdateIDs()
	if countIDs(ids, 10) != 1 || countIDs(ids, 11) != 1 || svc.botInstances["bot-synth"] != old {
		t.Fatalf("ids %v restored %v", ids, svc.botInstances["bot-synth"] == old)
	}
	runCancel()
	_ = old.StopAndWait(context.Background())
}

func TestStartBot_PublishFailureResumesOldPoll(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://gen-1.example/v1","apiKey":"gen-1","models":[{"id":"model-sol-main","temperature":0.2,"maxTokens":111}]}`)
	var active atomic.Int32
	var maxSeen atomic.Int32
	var phase atomic.Int32
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
		for {
			seen := maxSeen.Load()
			if n <= seen || maxSeen.CompareAndSwap(seen, n) {
				break
			}
		}
		defer active.Add(-1)
		if req.Offset == 0 && phase.Load() == 0 {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"text":"hi","chat":{"id":1,"type":"private"},"from":{"id":2}}}]}`))
			return
		}
		if req.Offset == 11 && phase.Load() == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":11,"message":{"message_id":2,"text":"next","chat":{"id":1,"type":"private"},"from":{"id":2}}}]}`))
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	logger := zap.NewNop().Sugar()
	tp := nooptrace.NewTracerProvider()
	ch := telegram.NewChannel("tg-old", "bot-synth", telegram.Config{
		Token: "123456:TEST", APIBaseURL: srv.URL, PollTimeout: 1, StreamReply: false,
	})
	pipe, err := pipeline.New(nil, tp, noopmetric.NewMeterProvider(), logger)
	if err != nil {
		t.Fatal(err)
	}
	old, err := bot.New(bot.BotParams{
		ID: "bot-synth", Logger: logger, TP: tp,
		Dispatcher: outbound.NewMultiDispatcher(logger, tp),
		Pipeline:   pipe, Channels: []bot.Channel{ch},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() { _ = old.Run(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(containsInt(ch.HandledUpdateIDs(), 10) && active.Load() >= 1) {
		time.Sleep(10 * time.Millisecond)
	}
	if !containsInt(ch.HandledUpdateIDs(), 10) {
		t.Fatal("old poller did not accept the first update")
	}
	svc := &BotService{
		db: db, store: st, logger: logger, tp: tp, mp: noopmetric.NewMeterProvider(),
		botInstances: map[string]*bot.Bot{"bot-synth": old},
		llmSlots:     map[string]*bot.LLMSlot{},
	}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	testPersistEffective = func([]string) error { return fmt.Errorf("disk full") }
	defer func() { testPersistEffective = nil }()
	phase.Store(1)
	err = svc.StartBot(context.Background(), "bot-synth")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("publish path: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !containsInt(ch.HandledUpdateIDs(), 11) {
		time.Sleep(10 * time.Millisecond)
	}
	if !containsInt(ch.HandledUpdateIDs(), 11) {
		t.Fatal("old poller was not resumed")
	}
	if maxSeen.Load() > 1 {
		t.Fatalf("overlapping getUpdates: %d", maxSeen.Load())
	}
	if svc.botInstances["bot-synth"] != old {
		t.Fatal("old instance was not restored")
	}
	runCancel()
	_ = old.StopAndWait(context.Background())
}

func countIDs(ids []int64, want int64) int {
	n := 0
	for _, id := range ids {
		if id == want {
			n++
		}
	}
	return n
}

func containsInt(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestStartBot_LateProviderRevisionKeepsHotClient(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://gen-1.example/v1","apiKey":"gen-1","models":[{"id":"model-sol-main","temperature":0.2,"maxTokens":111}]}`)
	rec := &paramProvider{}
	temp := 0.2
	slot := bot.NewLLMSlot(&bot.LLMSnapshot{
		Main: rec, MainModel: llm.ChatModel("model-sol-main"),
		MainDef: config.ModelDef{Model: "model-sol-main", MaxTokens: 111, Temperature: &temp},
	})
	keep := &bot.Bot{}
	svc := &BotService{
		db: db, store: st, logger: zap.NewNop().Sugar(),
		tp: nooptrace.NewTracerProvider(), mp: noopmetric.NewMeterProvider(),
		botInstances: map[string]*bot.Bot{"bot-synth": keep},
		llmSlots:     map[string]*bot.LLMSlot{"bot-synth": slot},
	}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	testBeforeCommit = func(string) {
		next := 0.9
		slot.Publish(&bot.LLMSnapshot{
			Main: rec, MainModel: llm.ChatModel("model-sol-main"),
			MainDef: config.ModelDef{Model: "model-sol-main", MaxTokens: 333, Temperature: &next},
		})
		body := `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://gen-2.example/v1","apiKey":"gen-2","models":[{"id":"model-sol-main","temperature":0.9,"maxTokens":333}]}`
		if err := st.Set(context.Background(), "provider.provider-synth-a", body); err != nil {
			t.Error(err)
		}
	}
	defer func() { testBeforeCommit = nil }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := svc.StartBot(ctx, "bot-synth")
	if err == nil {
		t.Fatal("stale client was published")
	}
	cur := slot.Current()
	if cur == nil || cur.MainDef.MaxTokens != 333 || cur.MainDef.Temperature == nil || *cur.MainDef.Temperature != 0.9 {
		t.Fatalf("hot client lost: %+v", cur)
	}
	if _, err := cur.Main.DoGenerate(context.Background(), llm.GenerateParams{MaxTokens: intPtr(cur.MainDef.MaxTokens), Temperature: cur.MainDef.Temperature}); err != nil {
		t.Fatal(err)
	}
	if rec.max != 333 || rec.temp != 0.9 {
		t.Fatalf("request used max %d temp %v", rec.max, rec.temp)
	}
	if svc.botInstances["bot-synth"] != keep {
		t.Fatal("running instance was replaced")
	}
}

func TestStartBot_ProviderRevisionKeepsOldClient(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main","temperature":0.2,"maxTokens":111}]}`)
	keep := &bot.Bot{}
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{"bot-synth": keep}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	testAfterPrepare = func(string) {
		body := `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://b.example/v1","apiKey":"dummy-token-beta-bbbb","models":[{"id":"model-sol-main","temperature":0.9,"maxTokens":222}]}`
		if err := st.Set(context.Background(), "provider.provider-synth-a", body); err != nil {
			t.Error(err)
		}
	}
	defer func() { testAfterPrepare = nil }()
	err := svc.StartBot(context.Background(), "bot-synth")
	if err == nil {
		t.Fatal("stale prepare was published")
	}
	if svc.botInstances["bot-synth"] != keep {
		t.Fatal("revision change replaced the running bot")
	}
}

func TestStartBot_StopDuringFailedStartDoesNotResurrect(t *testing.T) {
	db, st := applyDB(t)
	putProvider(t, st, "provider-synth-a", `{"id":"provider-synth-a","enabled":true,"clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","apiKey":"dummy-token-alpha-aaaa","models":[{"id":"model-sol-main"}]}`)
	keep := &bot.Bot{}
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{"bot-synth": keep}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-synth", Name: "synth", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	testFailStart = func(string) error {
		svc.StopBot("bot-synth")
		return fmt.Errorf("start failed after stop")
	}
	defer func() { testFailStart = nil }()
	if err := svc.StartBot(context.Background(), "bot-synth"); err == nil {
		t.Fatal("expected start failure")
	}
	if svc.botInstances["bot-synth"] != nil {
		t.Fatal("stopped bot was resurrected")
	}
}

func TestHandoverConsumers_NeverOverlaps(t *testing.T) {
	svc := &BotService{}
	var active atomic.Int32
	var maxSeen atomic.Int32
	active.Store(1)
	svc.handoverConsumers(func() {
		active.Add(-1)
	}, func() {
		n := active.Add(1)
		for {
			seen := maxSeen.Load()
			if n <= seen || maxSeen.CompareAndSwap(seen, n) {
				break
			}
		}
	})
	if maxSeen.Load() > 1 || active.Load() != 1 {
		t.Fatalf("active %d max %d", active.Load(), maxSeen.Load())
	}
}

func TestLLMForJob_InflightKeepsOldNextSeesNew(t *testing.T) {
	oldTemp := 0.2
	newTemp := 0.8
	rec := &paramProvider{}
	oldSnap := &bot.LLMSnapshot{
		Main: rec, MainModel: llm.ChatModel("model-old"),
		MainDef: config.ModelDef{Model: "model-old", MaxTokens: 111, Temperature: &oldTemp},
	}
	slot := bot.NewLLMSlot(oldSnap)
	wrapped := bot.FollowBundle(slot, &bot.LLMBundle{Main: rec, MainDef: oldSnap.MainDef})
	svc := &BotService{llmBundles: map[string]*bot.LLMBundle{"bot-synth": wrapped}, llmSlots: map[string]*bot.LLMSlot{"bot-synth": slot}}
	ctx, got, ok := svc.LLMForJob(context.Background(), "bot-synth")
	if !ok || got.MainDef.MaxTokens != 111 {
		t.Fatalf("job bundle max %d", got.MainDef.MaxTokens)
	}
	slot.Publish(&bot.LLMSnapshot{
		Main: rec, MainModel: llm.ChatModel("model-new"),
		MainDef: config.ModelDef{Model: "model-new", MaxTokens: 222, Temperature: &newTemp},
	})
	if _, err := got.Main.DoGenerate(ctx, llm.GenerateParams{MaxTokens: intPtr(9), Temperature: floatPtr(9)}); err != nil {
		t.Fatal(err)
	}
	if rec.max != 111 || rec.temp != 0.2 {
		t.Fatalf("inflight call max %d temp %v", rec.max, rec.temp)
	}
	ctx2, next, ok := svc.LLMForJob(context.Background(), "bot-synth")
	if !ok || next.MainDef.MaxTokens != 222 {
		t.Fatalf("next def max %d", next.MainDef.MaxTokens)
	}
	if _, err := next.Main.DoGenerate(ctx2, llm.GenerateParams{MaxTokens: intPtr(9), Temperature: floatPtr(9)}); err != nil {
		t.Fatal(err)
	}
	if rec.max != 222 || rec.temp != 0.8 {
		t.Fatalf("next call max %d temp %v", rec.max, rec.temp)
	}
}

func intPtr(n int) *int           { return &n }
func floatPtr(n float64) *float64 { return &n }

type paramProvider struct {
	max  int
	temp float64
}

func (p *paramProvider) Name() string { return "param" }
func (p *paramProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.MaxTokens != nil {
		p.max = *params.MaxTokens
	}
	if params.Temperature != nil {
		p.temp = *params.Temperature
	}
	return &llm.GenerateResult{Text: "ok"}, nil
}
func (p *paramProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, fmt.Errorf("not used")
}

func TestRefuseProviderRemoval_KeepsReferencedBots(t *testing.T) {
	db, st := applyDB(t)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar(), botInstances: map[string]*bot.Bot{}, llmSlots: map[string]*bot.LLMSlot{}}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-run", Name: "run", LLMMain: "model-sol-main", Status: dao.BotStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-stop", Name: "stop", LLMMain: "model-other", Status: dao.BotStatusStopped}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(context.Background(), config.BotLLMKey("bot-stop", "vision"), "model-sol-main"); err != nil {
		t.Fatal(err)
	}
	oldClient := &idProvider{id: "kept"}
	slot := bot.NewLLMSlot(&bot.LLMSnapshot{Main: oldClient, MainModel: llm.ChatModel("model-sol-main")})
	svc.llmSlots["bot-run"] = slot
	err := svc.RefuseProviderRemoval("provider-synth-a", []string{"model-sol-main"})
	if err == nil || !strings.Contains(err.Error(), "bot-run") || !strings.Contains(err.Error(), "bot-stop") {
		t.Fatalf("refuse %v", err)
	}
	if slot.Current().Main != oldClient {
		t.Fatal("refusal changed the client")
	}
	if err := svc.RefuseProviderRemoval("provider-synth-b", []string{"model-unused"}); err != nil {
		t.Fatal(err)
	}
}
