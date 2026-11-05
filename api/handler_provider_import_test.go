package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
)

// 远端 /models 响应在真实服务里至少有三种形状：只有 id、模态嵌在 architecture、
// 模态放在顶层并带 effort。以下样本按这三种真实结构裁剪而来，不含任何真实域名/凭据。

func TestParseRemoteModels_Shapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []remoteModelSpec
	}{
		{
			name: "flat modalities with effort",
			body: `{"object":"list","data":[{"id":"deepseek-flash","name":"Flash Model","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"}}]}`,
			want: []remoteModelSpec{{id: "deepseek-flash", name: "Flash Model", contextLength: 1048576, inputModalities: []string{"text", "image"}, reasoning: true}},
		},
		{
			name: "nested architecture with reasoning",
			body: `{"data":[{"id":"hy4","name":"Hy4","display_name":"Hy4 Display","context_length":1048576,"architecture":{"input_modalities":["text","image"]},"reasoning":{"default_effort":"high","supported_efforts":["no_think","high"]},"supported_parameters":["tools","reasoning"]}]}`,
			want: []remoteModelSpec{{id: "hy4", name: "Hy4", contextLength: 1048576, inputModalities: []string{"text", "image"}, reasoning: true}},
		},
		{
			name: "id only",
			body: `{"object":"list","data":[{"id":"glm-5.3","object":"model","created":1753632000,"owned_by":"vendor"}]}`,
			want: []remoteModelSpec{{id: "glm-5.3"}},
		},
		{
			name: "models key",
			body: `{"models":[{"id":"model-a"}]}`,
			want: []remoteModelSpec{{id: "model-a"}},
		},
		{
			name: "bare array of objects",
			body: `[{"id":"model-b","name":"Model B"}]`,
			want: []remoteModelSpec{{id: "model-b", name: "Model B"}},
		},
		{
			name: "bare array of strings",
			body: `["model-c","model-d"]`,
			want: []remoteModelSpec{{id: "model-c"}, {id: "model-d"}},
		},
		{
			name: "display_name fallback",
			body: `{"data":[{"id":"model-e","display_name":"Display E"}]}`,
			want: []remoteModelSpec{{id: "model-e", name: "Display E"}},
		},
		{
			name: "context as numeric string",
			body: `{"data":[{"id":"model-f","context_length":"262144"}]}`,
			want: []remoteModelSpec{{id: "model-f", contextLength: 262144}},
		},
		{
			name: "unexpected field type does not break the batch",
			body: `{"data":[{"id":"model-g","context_length":{"bad":1},"name":42},{"id":"model-h","name":"Model H","context_window":8192}]}`,
			want: []remoteModelSpec{{id: "model-g"}, {id: "model-h", name: "Model H", contextLength: 8192}},
		},
		{
			name: "reasoning declared only via supported_parameters",
			body: `{"data":[{"id":"model-i","supported_parameters":["tools","reasoning"]}]}`,
			want: []remoteModelSpec{{id: "model-i", reasoning: true}},
		},
		{
			name: "empty object",
			body: `{}`,
			want: []remoteModelSpec{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRemoteModels([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseRemoteModels: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("count: got %d want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if !reflect.DeepEqual(got[i], tc.want[i]) {
					t.Errorf("[%d] got %+v want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildImportedModel(t *testing.T) {
	cases := []struct {
		name string
		spec remoteModelSpec
		want ProviderModel
	}{
		{
			name: "image model with effort capability",
			spec: remoteModelSpec{id: "deepseek-flash", name: "Flash Model", contextLength: 1048576, inputModalities: []string{"text", "image"}, reasoning: true},
			want: ProviderModel{
				ID: "deepseek-flash", Name: "Flash Model", Capabilities: []string{"chat", "reasoning"},
				ContextLength: 1048576, Multimodal: true, Temperature: 1.0, TopP: 1.0, MaxTokens: 65536,
			},
		},
		{
			// 该 id 不在 modelPresetRules 前缀表内，采样/输出回退到内置兜底；
			// 这是项目既有约定（MaxTokens 不照搬官方 max_output_tokens 架构上限）。
			name: "image model with nested architecture",
			spec: remoteModelSpec{id: "hy4", name: "Hy4", contextLength: 1048576, inputModalities: []string{"text", "image"}, reasoning: true},
			want: ProviderModel{
				ID: "hy4", Name: "Hy4", Capabilities: []string{"chat", "reasoning"},
				ContextLength: 1048576, Multimodal: true, Temperature: 0.7, TopP: 0.0, MaxTokens: 4096,
			},
		},
		{
			name: "id only falls back to preset",
			spec: remoteModelSpec{id: "glm-5.3"},
			want: ProviderModel{
				ID: "glm-5.3", Name: "glm-5.3", Capabilities: []string{"chat"},
				ContextLength: 1_000_000, Multimodal: false, Temperature: 1.0, TopP: 0.95, MaxTokens: 65536,
			},
		},
		{
			name: "text-only model",
			spec: remoteModelSpec{id: "deepseek-v4-pro", contextLength: 1048576, inputModalities: []string{"text"}, reasoning: true},
			want: ProviderModel{
				ID: "deepseek-v4-pro", Name: "deepseek-v4-pro", Capabilities: []string{"chat", "reasoning"},
				ContextLength: 1048576, Multimodal: false, Temperature: 1.0, TopP: 1.0, MaxTokens: 65536,
			},
		},
		{
			name: "unknown model with explicit context",
			spec: remoteModelSpec{id: "vendor-custom-1", contextLength: 32768},
			want: ProviderModel{
				ID: "vendor-custom-1", Name: "vendor-custom-1", Capabilities: []string{"chat"},
				ContextLength: 32768, Multimodal: false, Temperature: 0.7, TopP: 0.0, MaxTokens: 4096,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildImportedModel(tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			// 架构上限（示例里为 393216）绝不能被当成默认输出预算。
			if got.MaxTokens == 393216 {
				t.Fatalf("MaxTokens must not be copied from max_output_tokens: %d", got.MaxTokens)
			}
		})
	}
}

// TestHandleImportModels_EndToEnd 验证整条链路：假 /models → handler → 落库，
// 以及「只新增不覆盖」不会把用户手工修正过的规格导回默认值。
func TestHandleImportModels_EndToEnd(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"deepseek-flash","name":"Flash Model","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"}},
			{"id":"deepseek-v4-pro","name":"Pro Model","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"}}
		]}`))
	}))
	defer remote.Close()

	store := config.NewStore(nil)
	srv := &Server{store: store, logger: zap.NewNop().Sugar()}
	pid := "test-provider"
	def := ProviderDef{
		ID: pid, Name: "Test Provider", ClientType: "OpenAI Compatible",
		BaseURL: remote.URL, APIKey: "test-key", Enabled: false, Models: []ProviderModel{},
	}
	writeTestProvider(t, store, def)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/providers/:pid/models/import", srv.handleImportModels)

	call := func() []ProviderModel {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/providers/"+pid+"/models/import", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Code != 0 {
			t.Fatalf("response code %d: %s", resp.Code, w.Body.String())
		}
		var models []ProviderModel
		if err := json.Unmarshal(resp.Data, &models); err != nil {
			t.Fatalf("decode data: %v", err)
		}
		return models
	}

	added := call()
	if len(added) != 2 {
		t.Fatalf("expected 2 imported models, got %d", len(added))
	}
	if added[0].Name != "Flash Model" || !added[0].Multimodal || added[0].ContextLength != 1048576 {
		t.Fatalf("image model spec wrong: %+v", added[0])
	}
	if !reflect.DeepEqual(added[0].Capabilities, []string{"chat", "reasoning"}) {
		t.Fatalf("reasoning capability missing: %+v", added[0].Capabilities)
	}
	if added[0].MaxTokens == 393216 {
		t.Fatalf("max_output_tokens must not become MaxTokens: %d", added[0].MaxTokens)
	}
	if added[1].Multimodal {
		t.Fatalf("text-only model must not be multimodal: %+v", added[1])
	}

	stored := readTestProvider(t, store, pid)
	if len(stored.Models) != 2 {
		t.Fatalf("stored model count = %d", len(stored.Models))
	}

	// 模拟用户手工修正：改掉第一个模型的名字与多模态开关，再次导入不应覆盖。
	stored.Models[0].Name = "manually-fixed"
	stored.Models[0].Multimodal = false
	writeTestProvider(t, store, stored)

	if again := call(); len(again) != 0 {
		t.Fatalf("second import should add nothing, got %d", len(again))
	}
	final := readTestProvider(t, store, pid)
	if len(final.Models) != 2 || final.Models[0].Name != "manually-fixed" {
		t.Fatalf("existing model was overwritten: %+v", final.Models)
	}
}

func writeTestProvider(t *testing.T, store *config.Store, def ProviderDef) {
	t.Helper()
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), providerConfigKey(def.ID), string(data)); err != nil {
		t.Fatal(err)
	}
}

func readTestProvider(t *testing.T, store *config.Store, pid string) ProviderDef {
	t.Helper()
	raw, ok := store.Get(providerConfigKey(pid))
	if !ok {
		t.Fatalf("provider %s not stored", pid)
	}
	var def ProviderDef
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatal(err)
	}
	return def
}
