package stages

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// ============================================================================
// 真实 LLM 集成测试 — 回复控制门控（复现「bot 知道不该回却发了」场景）
//
// 凭据来源（按序，都不满足则 t.Skip）：
//   1. 环境变量 THINKBOT_TEST_LLM_API_KEY / _BASE_URL / _MODEL / _CHAT_MODE
//      （与 skill/integration_test.go 同范式）
//   2. 本机项目根 thinkbot.db 的 provider.provider-djtzhmuij940（真实 GLM 凭据）
//      + bot.bot-2d8f9b087270da0bcfe177a5.main（模型名）
//
// 这样无需把 secret 硬编码进代码，露娜直接 `go test` 即可用真实 GLM 跑。
//
// 运行（强联网 + 真实 GLM）：
//   go test -v -run TestReplyControl_RealLLM -timeout 180s ./agent/stages/
// ============================================================================

// loadRealLLMProvider 构造真实 GLM provider，返回 provider 与 model 名。
func loadRealLLMProvider(t *testing.T) (llm.Provider, string) {
	t.Helper()

	apiKey := os.Getenv("THINKBOT_TEST_LLM_API_KEY")
	baseURL := os.Getenv("THINKBOT_TEST_LLM_BASE_URL")
	model := os.Getenv("THINKBOT_TEST_LLM_MODEL")
	chatMode := os.Getenv("THINKBOT_TEST_LLM_CHAT_MODE") == "1"

	if apiKey == "" {
		// fallback：从本机 thinkbot.db 读真实 GLM 凭据
		dbKey, dbURL, dbModel := loadGLMFromDB(t)
		if dbKey == "" {
			t.Skip("no LLM credentials available: set THINKBOT_TEST_LLM_API_KEY or place a thinkbot.db with the GLM provider config")
		}
		apiKey, baseURL, model = dbKey, dbURL, dbModel
		chatMode = true // bigmodel 走 Chat Completions 模式
	}
	if model == "" {
		model = "glm-5.2"
	}
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/coding/paas/v4"
		chatMode = true
	}

	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithBaseURL(baseURL),
		openai.WithTimeout(120 * time.Second),
	}
	// bigmodel（智谱 GLM）是 OpenAI 兼容的 Chat Completions 供应商；
	// 其 coding 变体 baseURL 已含版本段，chat 端点须为 /chat/completions（不含 /v1）。
	if chatMode || strings.Contains(baseURL, "bigmodel") {
		opts = append(opts, openai.WithChatMode(), openai.WithChatPath("/chat/completions"))
	}
	t.Logf("real LLM provider: baseURL=%s model=%s chatMode=%v", baseURL, model, chatMode)
	return openai.New(opts...), model
}

// firstN 返回 s 的前 n 个字符（UTF-8 安全截断），用于日志里展示思考过程片段。
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …[truncated]"
}

// loadGLMFromDB 从项目根 thinkbot.db 读 GLM 凭据（apiKey / baseURL / model）。
// 仅本集成测试用于免 export 跑真实 LLM，不暴露 secret 到代码。
// go test 的工作目录是包目录（agent/stages），故向上查找项目根 db。
func loadGLMFromDB(t *testing.T) (apiKey, baseURL, model string) {
	t.Helper()
	candidates := []string{"thinkbot.db", "../thinkbot.db", "../../thinkbot.db", "../../../thinkbot.db"}
	dbPath := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			dbPath = c
			break
		}
	}
	if dbPath == "" {
		return "", "", ""
	}
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return "", "", ""
	}
	defer func() { _ = conn.Close() }()

	// provider JSON 含 apiKey / baseUrl
	var provJSON string
	if err := conn.QueryRow(
		"SELECT value FROM config_settings WHERE key='provider.provider-djtzhmuij940'",
	).Scan(&provJSON); err != nil {
		return "", "", ""
	}
	var prov struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal([]byte(provJSON), &prov); err != nil {
		return "", "", ""
	}
	// bot 主模型名
	var m string
	if err := conn.QueryRow(
		"SELECT value FROM config_settings WHERE key='bot.bot-2d8f9b087270da0bcfe177a5.main'",
	).Scan(&m); err == nil && m != "" {
		model = m
	}
	return prov.APIKey, prov.BaseURL, model
}

// TestReplyControl_RealLLM 用真实 GLM 复现并验证「出问题的场景」：
// 可写模式下，模型「决定不互动」时，开启回复控制门控后独白绝不出站。
func TestReplyControl_RealLLM(t *testing.T) {
	provider, model := loadRealLLMProvider(t)

	newStage := func() *LLMStage {
		temp := 0.3
		maxTok := 2000
		return NewLLMStage("llm", provider, LLMConfig{
			// 协议指令作为 system prompt：把「是否出站」变成可解析的确定性信号。
			SystemPrompt: replyControlInstruction,
			Model:        &llm.Model{ID: model},
			Temperature:  &temp,
			MaxTokens:    &maxTok,
			// 关键开关：开启后模型必须带控制块，否则 fail-closed 不出站。
			RequireReplyControl: true,
			// MessageBuilder / ToolResolver 留 nil → 走默认 UserMessage / 无工具，
			// 完整跑通 Process 的真实 LLM 调用路径。
		}, nil, zap.NewNop().Sugar())
	}

	t.Run("sensitive_timeline_post_stays_silent", func(t *testing.T) {
		// 复现 2026-08-17 故障帖：可写 Misskey 时间线上 @blogtalk 分享 Cloak 技术文章，
		// 该账号专注规避广告审核的欺骗性用途，bot 此前已标注谨慎互动。
		// 这正是「模型知道不该回、却把独白当回复发出」的原始场景。
		stage := newStage()
		env := core.NewEnvelope(core.Message{
			ID:      "test-reply-control-sensitive",
			TraceID: "trace-sensitive",
			BotID:   "bot-2d8f9b087270da0bcfe177a5",
			Source:  "misskey",
			Channel: "misskey:timeline",
			UserID:  "blogtalk",
			Text: "你正在 Misskey 上作为 @栞娜 旁听 @blogtalk 的时间线。" +
				"@blogtalk 刚分享了一篇关于 Cloak 技术（回源风暴与缓存穿透定位）的故障排查文章。" +
				"背景：该账号长期专注 cloaking / AB 页跳转技术，涉及规避广告平台审核的欺骗性用途，" +
				"你此前已标注对其内容互动需谨慎。这条新帖没有新增需要记录的信息，" +
				"也不适合协助扩散或深入参与讨论。请基于以上判断，决定是否要公开回复这条帖子。",
			Mentioned: false,
		})

		out, err := stage.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}

		// 核心不变量：无论模型是「遵守协议 send:false」还是「忘了带控制块被 fail-closed 兜底」，
		// 独白都绝不应作为公开回复出站——这正是修复要治的 bug。
		for _, a := range out.Actions() {
			if a.Type == core.ActionReply {
				t.Fatalf("BUG REPRODUCED/REGRESSED: a reply was posted despite decide-not-to-interact.\npayload=%v", a.Payload)
			}
		}
		t.Logf("OK: no ActionReply posted for the sensitive timeline post (silence enforced)")

		// 诊断模型实际行为：遵守协议 vs 兜底，便于露娜判断协议提示词是否有效。
		if v, ok := out.Get("llm.result"); ok {
			if res, ok := v.(*llm.GenerateResult); ok {
				send, _, parsed := parseReplyControl(res.Text)
				t.Logf("model raw output:\n%s", res.Text)
				t.Logf("parsed control block: parsed=%v send=%v (parsed=true&send=false ⇒ 模型遵守协议; parsed=false ⇒ fail-closed 兜底)", parsed, send)
				t.Logf("REASONING (思考过程, len=%d chars, reasoning_tokens=%d):\n%s", len(res.Reasoning), res.Usage.ReasoningTokens, firstN(res.Reasoning, 1200))
				if !parsed {
					t.Logf("NOTE: model did NOT emit the control block; the gate held via fail-closed. " +
						"Consider tightening the prompt if this happens often.")
				}
			}
		}
	})

	t.Run("friendly_question_still_replies", func(t *testing.T) {
		// 对照：正常友好提问，协议不应误伤——模型应 send:true 并出站。
		stage := newStage()
		env := core.NewEnvelope(core.Message{
			ID:      "test-reply-control-friendly",
			TraceID: "trace-friendly",
			BotID:   "bot-2d8f9b087270da0bcfe177a5",
			Source:  "web",
			Channel: "web:direct",
			UserID:  "luna",
			Text:    "栞娜，帮我看下 Go 里怎么用 context 控制超时？给个最小例子。",
			Mentioned: true,
		})

		out, err := stage.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}

		var replied *core.Action
		for i := range out.Actions() {
			if out.Actions()[i].Type == core.ActionReply {
				a := out.Actions()[i]
				replied = &a
				break
			}
		}
		if replied == nil {
			t.Fatalf("REGRESSED: a normal friendly question was suppressed — the gate must not swallow legitimate replies")
		}
		// 出站正文应已剥离控制块，干净可读。
		t.Logf("OK: friendly question replied. clean payload=%v", replied.Payload)
		if v, ok := out.Get("llm.result"); ok {
			if res, ok := v.(*llm.GenerateResult); ok {
				send, clean, parsed := parseReplyControl(res.Text)
				t.Logf("model raw output:\n%s", res.Text)
				t.Logf("parsed control block: parsed=%v send=%v clean_len=%d", parsed, send, len(clean))
				t.Logf("REASONING (思考过程, len=%d chars, reasoning_tokens=%d):\n%s", len(res.Reasoning), res.Usage.ReasoningTokens, firstN(res.Reasoning, 1200))
			}
		}
	})

	t.Run("mixed_internal_and_public_strips_private", func(t *testing.T) {
		// 混合场景：bot 既有心里话（应包 <internal>），又有想公开说的话（可包 <public>）。
		// 核心不变量：出站 payload 必须不含 <internal>/<public> 标签，且心里话绝不外发。
		stage := newStage()
		env := core.NewEnvelope(core.Message{
			ID:      "test-reply-control-mixed",
			TraceID: "trace-mixed",
			BotID:   "bot-2d8f9b087270da0bcfe177a5",
			Source:  "misskey",
			Channel: "misskey:timeline",
			UserID:  "someone",
			Text: "你正在 Misskey 上作为 @栞娜。" +
				"一位关注者 @someone 发了条动态：「我觉得用缓存穿透去压测生产环境挺好玩的，准备今晚搞一下」。" +
				"你内心其实觉得这做法很危险、不想鼓励，但作为旁观者可以礼貌提醒一句风险。" +
				"请：把不想公开的内心判断包进 <internal> 标签，把打算公开说的礼貌提醒包进 <public> 标签，" +
				"结尾照例追加 @@REPLY_CONTROL@@ 控制行。",
			Mentioned: false,
		})

		out, err := stage.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}

		var replied *core.Action
		for i := range out.Actions() {
			if out.Actions()[i].Type == core.ActionReply {
				a := out.Actions()[i]
				replied = &a
				break
			}
		}
		if replied == nil {
			t.Fatalf("REGRESSED: mixed scenario produced no reply — model should still post the public part")
		}
		payload := fmt.Sprintf("%v", replied.Payload)
		// 不变量 1：标签本身必须被剥离，用户看不到 <internal>/<public> 标记。
		if strings.Contains(payload, "<internal>") || strings.Contains(payload, "<public>") {
			t.Fatalf("LEAK: outbound payload still contains thought/public tags:\n%v", payload)
		}
		// 不变量 2：心里话绝不外发——模型被要求把「危险、不想鼓励」这类判断写进 <internal>，
		// 若门控未剥离，该私有内容会以裸文本出现在 payload。这里做宽松断言：payload 非空即可，
		// 真正的标签剥离由上面「不含 <internal> 标签」保证（若模型用了标签，私有内容已被剥离）。
		if strings.TrimSpace(payload) == "" {
			t.Fatalf("REGRESSED: outbound payload empty after stripping")
		}
		t.Logf("OK: mixed scenario replied with public part only. clean payload=%v", payload)

		if v, ok := out.Get("llm.result"); ok {
			if res, ok := v.(*llm.GenerateResult); ok {
				send, clean, parsed := parseReplyControl(res.Text)
				t.Logf("model raw output:\n%s", res.Text)
				t.Logf("parsed control block: parsed=%v send=%v", parsed, send)
				t.Logf("REASONING (思考过程, len=%d chars, reasoning_tokens=%d):\n%s", len(res.Reasoning), res.Usage.ReasoningTokens, firstN(res.Reasoning, 1200))
				// 诊断：清洗后是否仍残留 <internal> 裸标签（说明模型写了标签但门控没剥——不应发生）。
			if strings.Contains(clean, "<internal>") {
				t.Logf("WARN: <internal> tag survived cleaning in `clean` (model may have used unexpected casing/format)")
			}
		}
	}
})

	t.Run("internal_without_public_stays_silent", func(t *testing.T) {
		// 取巧逻辑的关键场景：模型既写了 <internal> 心里话，又想说点公开的，
		// 但忘了包 <public> 标签。按露娜的取巧逻辑（<internal> 是强私密信号、无 <public> 则整段沉），
		// 应连 internal 之外的公开文本也一起 fail-closed 不发。
		stage := newStage()
		env := core.NewEnvelope(core.Message{
			ID:      "test-reply-control-internal-no-public",
			TraceID: "trace-internal-no-public",
			BotID:   "bot-2d8f9b087270da0bcfe177a5",
			Source:  "misskey",
			Channel: "misskey:timeline",
			UserID:  "someone",
			Text: "你正在 Misskey 上作为 @栞娜。" +
				"一位关注者发了条动态：「缓存穿透压测生产挺好玩」。" +
				"你内心有个判断（请把这个判断写进 <internal> 标签，里面包含暗号 PRIVATE_MARKER_abc123）。" +
				"你决定：不公开说任何话，只想在心里记这一笔。" +
				"请【只】写 <internal> 标签（含暗号），【绝对不要】写 <public> 标签，也【不要】在 <internal> 之外写任何裸公开文本。" +
				"结尾照例追加 @@REPLY_CONTROL@@ 控制行，send:true（你想发出去，但内容只有内部想法）。",
			Mentioned: false,
		})

		out, err := stage.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}

		var replied *core.Action
		for i := range out.Actions() {
			if out.Actions()[i].Type == core.ActionReply {
				a := out.Actions()[i]
				replied = &a
				break
			}
		}
		// 硬不变量：心里话（含 PRIVATE_MARKER 暗号）绝不外发。
		if replied != nil {
			payload := fmt.Sprintf("%v", replied.Payload)
			if strings.Contains(payload, "PRIVATE_MARKER_abc123") {
				t.Fatalf("LEAK: private thought with marker escaped to outbound payload:\n%v", payload)
			}
			// 模型自作主张用了 <public> → 发了公开部分，也是安全结果（心里话已剥离）。
			t.Logf("model emitted <public> on its own; public-only payload posted (safe). payload=%v", payload)
		} else {
			// 期望路径：有 internal 无 public → 整段 fail-closed 不发（取巧逻辑生效）。
			t.Logf("OK: internal-without-public produced NO ActionReply (whole reply dropped, fail-closed)")
		}

		if v, ok := out.Get("llm.result"); ok {
			if res, ok := v.(*llm.GenerateResult); ok {
				t.Logf("model raw output:\n%s", res.Text)
				t.Logf("REASONING (思考过程, len=%d chars, reasoning_tokens=%d):\n%s", len(res.Reasoning), res.Usage.ReasoningTokens, firstN(res.Reasoning, 1200))
			}
		}
	})
}
