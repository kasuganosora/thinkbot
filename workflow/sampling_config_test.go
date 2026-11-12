package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/subagent"
)

const samplingDAG = `<result>{"nodes":[{"id":"n1","name":"a","task":"t","dependencies":[]}]}</result>`

func TestAnalyzerOmitsSamplingForOmitModelOnRetry(t *testing.T) {
	bodies := serveChat(t, func(n int) (int, string) {
		if n < 2 {
			return http.StatusOK, chatSSE("")
		}
		return http.StatusOK, chatSSE(samplingDAG)
	})
	omit := float64(llm.OmitSamplingParam)
	prov := followMain(t, bodies.client, "gpt-synth", &omit, &omit, 128)
	sa := subagent.NewSubAgentManager(prov, "gpt-synth")
	a := NewAnalyzer(sa, nil, EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerStuckTimeout: 2 * time.Second,
		BindLiveModel: func(ctx context.Context) (context.Context, string, int) {
			return ctx, "gpt-synth", 128
		},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Analyze(ctx, "split the work", false); err != nil {
		t.Fatal(err)
	}
	got := bodies.snapshot()
	if len(got) < 2 {
		t.Fatalf("attempts %d, want a retry", len(got))
	}
	for i, body := range got {
		assertNoSampling(t, i, body)
	}
}

func TestAnalyzerUsesModelTemperatureNotAnalyzerConfig(t *testing.T) {
	bodies := serveChat(t, func(int) (int, string) {
		return http.StatusOK, chatSSE(samplingDAG)
	})
	temp := 0.4
	top := 0.8
	prov := followMain(t, bodies.client, "gpt-synth", &temp, &top, 64)
	sa := subagent.NewSubAgentManager(prov, "gpt-synth")
	a := NewAnalyzer(sa, nil, EngineConfig{
		AnalyzerTemperature:  -1,
		AnalyzerStuckTimeout: 2 * time.Second,
		BindLiveModel: func(ctx context.Context) (context.Context, string, int) {
			return ctx, "gpt-synth", 64
		},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.Analyze(ctx, "split the work", false); err != nil {
		t.Fatal(err)
	}
	got := bodies.snapshot()
	if len(got) != 1 {
		t.Fatalf("attempts %d", len(got))
	}
	if !strings.Contains(got[0], `"temperature":0.4`) {
		t.Fatalf("model temperature missing: %s", got[0])
	}
	if !strings.Contains(got[0], `"top_p":0.8`) {
		t.Fatalf("model top_p missing: %s", got[0])
	}
	if strings.Contains(got[0], `"temperature":-1`) || strings.Contains(got[0], `"temperature":0.3`) || strings.Contains(got[0], `"temperature":0.7`) {
		t.Fatalf("analyzer config replaced the model temperature: %s", got[0])
	}
}

func TestHealOmitsSamplingForOmitModel(t *testing.T) {
	bodies := serveChat(t, func(int) (int, string) {
		return http.StatusOK, chatSSE(`<result>{"category":"other","confidence":0.2,"reason":"unclear","suggested_action":"escalate"}</result>`)
	})
	omit := float64(llm.OmitSamplingParam)
	prov := followMain(t, bodies.client, "gpt-synth", &omit, &omit, 64)
	sa := subagent.NewSubAgentManager(prov, "gpt-synth")
	a := NewAnalyzer(sa, nil, EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerStuckTimeout: 2 * time.Second,
		BindLiveModel: func(ctx context.Context) (context.Context, string, int) {
			return ctx, "gpt-synth", 64
		},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.DiagnoseNode(ctx, &DAGNode{ID: "n1", Name: "n", Task: "read", Error: "timeout"}, &Workflow{Requirement: "work"}); err != nil {
		t.Fatal(err)
	}
	refineBodies := serveChat(t, func(int) (int, string) {
		return http.StatusOK, chatSSE(samplingDAG)
	})
	refineProv := followMain(t, refineBodies.client, "gpt-synth", &omit, &omit, 64)
	sa2 := subagent.NewSubAgentManager(refineProv, "gpt-synth")
	a2 := NewAnalyzer(sa2, nil, EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerStuckTimeout: 2 * time.Second,
		BindLiveModel: func(ctx context.Context) (context.Context, string, int) {
			return ctx, "gpt-synth", 64
		},
	}, nil)
	if _, err := a2.RefineNode(ctx, &Workflow{Requirement: "work"}, &DAGNode{ID: "n1", Name: "n", Task: "read"}, "split"); err != nil {
		t.Fatal(err)
	}
	for i, body := range append(bodies.snapshot(), refineBodies.snapshot()...) {
		assertNoSampling(t, i, body)
	}
}

func assertNoSampling(t *testing.T, i int, body string) {
	t.Helper()
	if strings.Contains(body, `"temperature"`) || strings.Contains(body, `"top_p"`) {
		t.Fatalf("attempt %d sent sampling params", i)
	}
}

type chatCapture struct {
	client *openai.Client
	mu     sync.Mutex
	bodies []string
}

func (c *chatCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return out
}

func serveChat(t *testing.T, reply func(n int) (int, string)) *chatCapture {
	t.Helper()
	cap := &chatCapture{}
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, string(b))
		n++
		call := n
		cap.mu.Unlock()
		status, payload := reply(call)
		if status == 0 {
			status = http.StatusOK
		}
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	cap.client = openai.New(
		openai.WithAPIKey("dummy-token-not-logged"),
		openai.WithBaseURL(srv.URL+"/v1"),
		openai.WithChatMode(),
		openai.WithChatPath("/chat/completions"),
		openai.WithTimeout(2*time.Second),
	)
	return cap
}

func chatSSE(content string) string {
	raw, _ := json.Marshal(content)
	return "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":" + string(raw) + "}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
}

func followMain(t *testing.T, client llm.Provider, modelID string, temp, top *float64, maxTok int) llm.Provider {
	t.Helper()
	model := llm.ChatModel(modelID)
	model.Temperature = temp
	snap := &bot.LLMSnapshot{
		Main:      client,
		MainModel: model,
		MainDef: config.ModelDef{
			Model:       modelID,
			Temperature: temp,
			TopP:        top,
			MaxTokens:   maxTok,
		},
	}
	slot := bot.NewLLMSlot(snap)
	return bot.FollowBundle(slot, &bot.LLMBundle{Main: client, MainDef: snap.MainDef}).Main
}

func TestTaskToolFailureIsStructuredError(t *testing.T) {
	prev := analyzerRetryWait
	analyzerRetryWait = func(int) time.Duration { return 0 }
	t.Cleanup(func() { analyzerRetryWait = prev })

	const leaked = "do-not-echo-prompt sk-test-secret"
	const requirement = "synthetic-task-phrase"
	bodies := serveChat(t, func(int) (int, string) {
		raw := `{"error":{"message":"` + leaked + `","type":"invalid_request_error","code":"invalid_request_error","param":"temperature"}}`
		return http.StatusBadRequest, raw
	})
	omit := float64(llm.OmitSamplingParam)
	prov := followMain(t, bodies.client, "gpt-synth", &omit, &omit, 32)
	sa := subagent.NewSubAgentManager(prov, "gpt-synth")
	analyzer := NewAnalyzer(sa, nil, EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerStuckTimeout: time.Second,
		BindLiveModel: func(ctx context.Context) (context.Context, string, int) {
			return ctx, "gpt-synth", 32
		},
	}, zap.NewNop().Sugar())
	mgr := NewManager(NewRepository(nil, nil), analyzer, nil, nil, EngineConfig{}, nil, nil)
	tool := submitToolDef(mgr).Tool
	caller := &scriptedToolCaller{}
	res, err := llm.OrchestrateGenerate(context.Background(), caller, &llm.OrchestrateConfig{
		MaxSteps: 2,
		Params: llm.GenerateParams{
			Model:    llm.ChatModel("gpt-synth"),
			Messages: []llm.Message{llm.UserMessage("go")},
			Tools:    []llm.Tool{tool},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var isError bool
	for _, msg := range res.Messages {
		for _, part := range msg.Content {
			tr, ok := part.(llm.ToolResultPart)
			if !ok || tr.ToolName != "task" {
				continue
			}
			isError = tr.IsError
			text = strings.TrimSpace(stringifyTool(tr.Result))
		}
	}
	if !isError {
		t.Fatalf("is_error=false text=%q", text)
	}
	if strings.Contains(text, "&{") || strings.Contains(text, leaked) || strings.Contains(text, requirement) {
		t.Fatalf("unreadable or unsanitized tool error: %s", text)
	}
	for _, want := range []string{"status=failed", "waited=", "provider request failed with HTTP 400", "param=temperature", "type=invalid_request_error"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for i, body := range bodies.snapshot() {
		assertNoSampling(t, i, body)
	}
}

func stringifyTool(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type scriptedToolCaller struct{ n int }

func (p *scriptedToolCaller) Name() string { return "script" }
func (p *scriptedToolCaller) DoGenerate(_ context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	p.n++
	if p.n == 1 {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{{
				ToolCallID: "call-1",
				ToolName:   "task",
				Input:      map[string]any{"requirement": "synthetic-task-phrase"},
			}},
		}, nil
	}
	return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "noted"}, nil
}
func (p *scriptedToolCaller) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errString("stream not used")
}

type errString string

func (e errString) Error() string { return string(e) }
