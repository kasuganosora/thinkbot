package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
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
