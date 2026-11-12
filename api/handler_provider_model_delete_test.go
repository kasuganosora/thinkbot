package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestHandleDeleteModel_ReferencedModelIsUnchanged(t *testing.T) {
	db, st := applyDB(t)
	svc := &BotService{db: db, store: st, logger: zap.NewNop().Sugar()}
	if err := svc.CreateDefinition(&dao.BotDefinition{ID: "bot-stop", Name: "stop", LLMMain: "model-sol-main", Status: dao.BotStatusStopped}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st, botSvc: svc, logger: zap.NewNop().Sugar()}
	writeTestProvider(t, st, ProviderDef{
		ID: "provider-synth-a", Name: "synth", ClientType: "OpenAI Compatible",
		BaseURL: "http://a.example/v1", APIKey: "dummy-token-alpha-aaaa", Enabled: true,
		Models: []ProviderModel{{ID: "model-sol-main", Name: "sol"}, {ID: "model-other", Name: "other"}},
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/api/providers/:pid/models/:mid", srv.handleDeleteModel)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/providers/provider-synth-a/models/model-sol-main", nil)
	router.ServeHTTP(w, req)
	if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "still selected") {
		t.Fatalf("delete succeeded: %d %s", w.Code, w.Body.String())
	}
	raw, _ := st.Get("provider.provider-synth-a")
	if !strings.Contains(raw, "model-sol-main") {
		t.Fatalf("model removed: %s", raw)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] == float64(0) {
		t.Fatalf("success body %s", w.Body.String())
	}
}
