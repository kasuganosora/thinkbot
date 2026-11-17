package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestListProvidersStableOrderAcrossUpdates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, st := applyDB(t)
	srv := &Server{store: st, logger: zap.NewNop().Sugar()}
	r := gin.New()
	r.GET("/api/providers", srv.handleListProviders)
	r.POST("/api/providers", srv.handleCreateProvider)
	r.PUT("/api/providers/:pid", srv.handleUpdateProvider)
	r.POST("/api/providers/:pid/models", srv.handleAddModel)
	r.PUT("/api/providers/:pid/models/:mid", srv.handleUpdateModel)

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

	listProviders := func() []ProviderResp {
		t.Helper()
		w := call(http.MethodGet, "/api/providers", "")
		if w.Code != http.StatusOK {
			t.Fatalf("list %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data []ProviderResp `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal list: %v", err)
		}
		return resp.Data
	}

	ids := []string{
		"provider-synth-a",
		"provider-synth-b",
		"provider-synth-c",
		"provider-synth-d",
		"provider-synth-e",
		"provider-synth-f",
	}
	for i, id := range ids {
		body := fmt.Sprintf(`{"id":%q,"name":"Service %d","clientType":"OpenAI Compatible","baseUrl":"http://p%d.example/v1"}`, id, i, i)
		if w := call(http.MethodPost, "/api/providers", body); w.Code != http.StatusOK {
			t.Fatalf("create %s: %d %s", id, w.Code, w.Body.String())
		}
	}

	extractIDs := func(items []ProviderResp) []string {
		out := make([]string, len(items))
		for i, item := range items {
			out[i] = item.ID
		}
		return out
	}

	baseline := extractIDs(listProviders())
	if !reflect.DeepEqual(baseline, ids) {
		t.Fatalf("initial order = %v, want %v", baseline, ids)
	}

	// Repeated GET /api/providers calls must never shuffle due to Go map iteration.
	for i := 0; i < 25; i++ {
		got := extractIDs(listProviders())
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("iteration %d shuffled provider order: got %v, want %v", i, got, baseline)
		}
	}

	// Renaming and editing a middle provider must preserve order while surfacing real updates.
	if w := call(http.MethodPost, "/api/providers/provider-synth-c/models", `{"id":"model-1","name":"Model One","temperature":0.6}`); w.Code != http.StatusOK {
		t.Fatalf("add model: %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPut, "/api/providers/provider-synth-c", `{"name":"ZZZ Renamed Service","baseUrl":"http://updated.example/v1","enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("update provider: %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPut, "/api/providers/provider-synth-c/models/model-1", `{"temperature":-1}`); w.Code != http.StatusOK {
		t.Fatalf("update model: %d %s", w.Code, w.Body.String())
	}

	for i := 0; i < 15; i++ {
		items := listProviders()
		got := extractIDs(items)
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("post-update iteration %d changed order: got %v, want %v", i, got, baseline)
		}
		target := items[2]
		if target.ID != "provider-synth-c" || target.Name != "ZZZ Renamed Service" || target.BaseURL != "http://updated.example/v1" || !target.Enabled {
			t.Fatalf("updated provider masked or misplaced: %+v", target)
		}
		if len(target.Models) != 1 || target.Models[0].Temperature != -1 {
			t.Fatalf("updated model masked: %+v", target.Models)
		}
	}
}
