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

func TestProviderTokenParamStyleRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, st := applyDB(t)
	srv := &Server{store: st, logger: zap.NewNop().Sugar()}
	r := gin.New()
	r.POST("/api/providers", srv.handleCreateProvider)
	r.PUT("/api/providers/:pid", srv.handleUpdateProvider)
	r.GET("/api/providers/:pid", func(c *gin.Context) {
		def, err := srv.getProvider(c.Param("pid"))
		if err != nil {
			Fail(c, err)
			return
		}
		OK(c, toProviderResp(def))
	})

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	styleOf := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var resp struct {
			Data struct {
				TokenParamStyle string `json:"tokenParamStyle"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json %v body %s", err, w.Body.String())
		}
		return resp.Data.TokenParamStyle
	}

	w := call(http.MethodPost, "/api/providers", `{"id":"provider-synth-a","name":"synth","clientType":"OpenAI Compatible","baseUrl":"http://a.example/v1","tokenParamStyle":"max_completion_tokens"}`)
	if w.Code != http.StatusOK || styleOf(w) != "max_completion_tokens" {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"name":"renamed"}`)
	if w.Code != http.StatusOK || styleOf(w) != "max_completion_tokens" {
		t.Fatalf("omit update %d %s", w.Code, w.Body.String())
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"tokenParamStyle":"AUTO"}`)
	if w.Code != http.StatusOK || styleOf(w) != "auto" {
		t.Fatalf("auto update %d %s", w.Code, w.Body.String())
	}
	w = call(http.MethodPut, "/api/providers/provider-synth-a", `{"tokenParamStyle":"nope"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("invalid style accepted: %s", w.Body.String())
	}
	w = call(http.MethodGet, "/api/providers/provider-synth-a", "")
	if styleOf(w) != "auto" {
		t.Fatalf("style changed after reject: %s", w.Body.String())
	}
}
