package prompt

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Variable 测试
// ============================================================================

func TestVariableSource_String(t *testing.T) {
	tests := []struct {
		source VariableSource
		want   string
	}{
		{SourceStatic, "static"},
		{SourceEnvelopeKV, "envelope_kv"},
		{SourceFunc, "func"},
		{VariableSource(99), "unknown(99)"},
	}
	for _, tt := range tests {
		if got := tt.source.String(); got != tt.want {
			t.Errorf("VariableSource(%d).String() = %q, want %q", int(tt.source), got, tt.want)
		}
	}
}

// ============================================================================
// AssemblyContext 测试
// ============================================================================

func TestAssemblyContext_GetString(t *testing.T) {
	ctx := &AssemblyContext{
		Values: map[string]any{
			"str_key": "hello",
			"int_key": 42,
		},
	}

	if got := ctx.GetString("str_key"); got != "hello" {
		t.Errorf("GetString(str_key) = %q, want %q", got, "hello")
	}
	if got := ctx.GetString("int_key"); got != "" {
		t.Errorf("GetString(int_key) = %q, want empty", got)
	}
	if got := ctx.GetString("missing"); got != "" {
		t.Errorf("GetString(missing) = %q, want empty", got)
	}

	// nil values
	nilCtx := &AssemblyContext{}
	if got := nilCtx.GetString("any"); got != "" {
		t.Errorf("GetString on nil Values = %q, want empty", got)
	}
}

func TestAssemblyContext_GetInt(t *testing.T) {
	ctx := &AssemblyContext{
		Values: map[string]any{
			"int_key":   42,
			"int64_key": int64(100),
			"str_key":   "not_an_int",
		},
	}

	if v, ok := ctx.GetInt("int_key"); !ok || v != 42 {
		t.Errorf("GetInt(int_key) = %d, %v, want 42, true", v, ok)
	}
	if v, ok := ctx.GetInt("int64_key"); !ok || v != 100 {
		t.Errorf("GetInt(int64_key) = %d, %v, want 100, true", v, ok)
	}
	if _, ok := ctx.GetInt("str_key"); ok {
		t.Errorf("GetInt(str_key) should return false")
	}
	if _, ok := ctx.GetInt("missing"); ok {
		t.Errorf("GetInt(missing) should return false")
	}
}

func TestAssemblyContext_GetBool(t *testing.T) {
	ctx := &AssemblyContext{
		Values: map[string]any{
			"bool_key": true,
			"str_key":  "true",
		},
	}

	if v, ok := ctx.GetBool("bool_key"); !ok || !v {
		t.Errorf("GetBool(bool_key) = %v, %v, want true, true", v, ok)
	}
	if _, ok := ctx.GetBool("str_key"); ok {
		t.Errorf("GetBool(str_key) should return false")
	}
}

// ============================================================================
// Registry 测试
// ============================================================================

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()

	sec := Section{
		Name:    "identity",
		Order:   0,
		Content: "You are a helpful assistant.",
		Enabled: true,
	}
	reg.Register(sec)

	got, ok := reg.Get("identity")
	if !ok {
		t.Fatal("Get(identity) returned false")
	}
	if got.Content != sec.Content {
		t.Errorf("Content = %q, want %q", got.Content, sec.Content)
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d, want 1", reg.Len())
	}
}

func TestRegistry_RegisterOverwrite(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{Name: "foo", Content: "v1", Enabled: true})
	reg.Register(Section{Name: "foo", Content: "v2", Enabled: true})

	got, _ := reg.Get("foo")
	if got.Content != "v2" {
		t.Errorf("overwrite failed: Content = %q, want %q", got.Content, "v2")
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d after overwrite, want 1", reg.Len())
	}
}

func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{Name: "foo", Content: "bar", Enabled: true})
	reg.Unregister("foo")

	if _, ok := reg.Get("foo"); ok {
		t.Error("Get(foo) returned true after Unregister")
	}
	if reg.Len() != 0 {
		t.Errorf("Len() = %d after Unregister, want 0", reg.Len())
	}

	// 注销不存在的 section 不 panic
	reg.Unregister("nonexistent")
}

func TestRegistry_List_SortedByOrder(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{Name: "c", Order: 300, Content: "c", Enabled: true},
		Section{Name: "a", Order: 100, Content: "a", Enabled: true},
		Section{Name: "b", Order: 200, Content: "b", Enabled: true},
	)

	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("List() len = %d, want 3", len(list))
	}
	if list[0].Name != "a" || list[1].Name != "b" || list[2].Name != "c" {
		t.Errorf("List() order = [%s, %s, %s], want [a, b, c]", list[0].Name, list[1].Name, list[2].Name)
	}
}

func TestRegistry_Metrics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{Name: "a", Enabled: true})
	reg.Register(Section{Name: "b", Enabled: true})
	reg.Unregister("a")

	m := reg.Metrics()
	if m.Registered != 2 {
		t.Errorf("Registered = %d, want 2", m.Registered)
	}
	if m.Unregistered != 1 {
		t.Errorf("Unregistered = %d, want 1", m.Unregistered)
	}
	if m.CurrentSize != 1 {
		t.Errorf("CurrentSize = %d, want 1", m.CurrentSize)
	}
}

// ============================================================================
// Assembler 测试
// ============================================================================

func TestAssembler_BasicAssembly(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{
			Name:    "identity",
			Order:   0,
			Content: "You are Bot-X.",
			Enabled: true,
		},
		Section{
			Name:    "rules",
			Order:   100,
			Content: "Be helpful and concise.",
			Enabled: true,
		},
	)

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	ctx := &AssemblyContext{Timestamp: time.Now()}

	result, err := asm.Assemble(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Prompt != "You are Bot-X.\n\nBe helpful and concise." {
		t.Errorf("Prompt = %q", result.Prompt)
	}
	if len(result.SectionsUsed) != 2 {
		t.Errorf("SectionsUsed = %v", result.SectionsUsed)
	}
	if result.PromptLength != len(result.Prompt) {
		t.Errorf("PromptLength = %d, actual = %d", result.PromptLength, len(result.Prompt))
	}
}

func TestAssembler_VariableResolution(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "greeting",
		Order:   0,
		Content: "Hello, {{.UserName}}! You are in {{.Channel}}.",
		Enabled: true,
		Variables: []Variable{
			{Name: "UserName", Source: SourceStatic, StaticValue: "Alice"},
			{Name: "Channel", Source: SourceEnvelopeKV, EnvelopeKey: "current_channel"},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	ctx := &AssemblyContext{
		Values: map[string]any{
			"current_channel": "#general",
		},
	}

	result, err := asm.Assemble(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := "Hello, Alice! You are in #general."
	if result.Prompt != expected {
		t.Errorf("Prompt = %q, want %q", result.Prompt, expected)
	}
	if result.VariablesResolved != 2 {
		t.Errorf("VariablesResolved = %d, want 2", result.VariablesResolved)
	}
}

func TestAssembler_FuncVariable(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "dynamic",
		Order:   0,
		Content: "Current time: {{.Time}}",
		Enabled: true,
		Variables: []Variable{
			{
				Name:   "Time",
				Source: SourceFunc,
				Func: func(ctx *AssemblyContext) string {
					return ctx.Timestamp.Format("2006-01-02")
				},
			},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	ctx := &AssemblyContext{Timestamp: now}

	result, err := asm.Assemble(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := "Current time: 2026-06-18"
	if result.Prompt != expected {
		t.Errorf("Prompt = %q, want %q", result.Prompt, expected)
	}
}

func TestAssembler_ConditionalSection(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{
			Name:    "base",
			Order:   0,
			Content: "Base prompt.",
			Enabled: true,
		},
		Section{
			Name:    "group_rules",
			Order:   100,
			Content: "In group chats, only respond when mentioned.",
			Enabled: true,
			Conditional: func(ctx *AssemblyContext) bool {
				return ctx.ChatType == "group"
			},
		},
	)

	asm := NewAssembler(reg, DefaultAssemblerConfig())

	// 群聊场景
	result, _ := asm.Assemble(&AssemblyContext{ChatType: "group"})
	if len(result.SectionsUsed) != 2 {
		t.Errorf("group: SectionsUsed = %v, want 2 sections", result.SectionsUsed)
	}

	// 私聊场景
	result, _ = asm.Assemble(&AssemblyContext{ChatType: "private"})
	if len(result.SectionsUsed) != 1 {
		t.Errorf("private: SectionsUsed = %v, want 1 section", result.SectionsUsed)
	}
	if len(result.SectionsSkipped) != 1 || result.SectionsSkipped[0] != "group_rules" {
		t.Errorf("private: SectionsSkipped = %v, want [group_rules]", result.SectionsSkipped)
	}
}

func TestAssembler_DisabledSection(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{Name: "a", Order: 0, Content: "A", Enabled: true},
		Section{Name: "b", Order: 100, Content: "B", Enabled: false},
	)

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, _ := asm.Assemble(&AssemblyContext{})

	if result.Prompt != "A" {
		t.Errorf("Prompt = %q, want %q", result.Prompt, "A")
	}
	if len(result.SectionsSkipped) != 1 {
		t.Errorf("SectionsSkipped = %v", result.SectionsSkipped)
	}
}

func TestAssembler_TrimEmpty(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{
			Name:    "a",
			Order:   0,
			Content: "Solid content.",
			Enabled: true,
		},
		Section{
			Name:    "empty",
			Order:   100,
			Content: "{{.Missing}}",
			Enabled: true,
			Variables: []Variable{
				{Name: "Missing", Source: SourceEnvelopeKV, EnvelopeKey: "not_here"},
			},
		},
	)

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, _ := asm.Assemble(&AssemblyContext{Values: map[string]any{}})

	if result.Prompt != "Solid content." {
		t.Errorf("Prompt = %q, want %q", result.Prompt, "Solid content.")
	}
}

func TestAssembler_StrictMode_RequiredVarFails(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "strict",
		Order:   0,
		Content: "Hello {{.Name}}",
		Enabled: true,
		Variables: []Variable{
			{Name: "Name", Source: SourceEnvelopeKV, EnvelopeKey: "user_name", Required: true},
		},
	})

	config := DefaultAssemblerConfig()
	config.StrictMode = true
	asm := NewAssembler(reg, config)

	_, err := asm.Assemble(&AssemblyContext{Values: map[string]any{}})
	if err == nil {
		t.Fatal("expected error for missing required variable in strict mode")
	}
}

func TestAssembler_DefaultValue(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "with_default",
		Order:   0,
		Content: "Name: {{.Name}}",
		Enabled: true,
		Variables: []Variable{
			{Name: "Name", Source: SourceEnvelopeKV, EnvelopeKey: "user_name", Default: "Anonymous"},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, _ := asm.Assemble(&AssemblyContext{Values: map[string]any{}})

	if result.Prompt != "Name: Anonymous" {
		t.Errorf("Prompt = %q, want %q", result.Prompt, "Name: Anonymous")
	}
	if result.VariablesFailed != 1 {
		t.Errorf("VariablesFailed = %d, want 1", result.VariablesFailed)
	}
}

func TestAssembler_MaxPromptLength_Truncation(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{Name: "a", Order: 0, Content: "AAAAAAAAAA", Enabled: true},   // 10 chars
		Section{Name: "b", Order: 100, Content: "BBBBBBBBBB", Enabled: true}, // 10 chars
		Section{Name: "c", Order: 200, Content: "CCCCCCCCCC", Enabled: true}, // 10 chars
	)

	config := DefaultAssemblerConfig()
	config.MaxPromptLength = 25 // "AAAAAAAAAA\n\nBBBBBBBBBB" = 22 chars
	asm := NewAssembler(reg, config)
	result, _ := asm.Assemble(&AssemblyContext{})

	if result.Truncated != true {
		t.Error("expected Truncated = true")
	}
	if len(result.Prompt) > 25 {
		t.Errorf("Prompt len = %d, want <= 25", len(result.Prompt))
	}
	// "c" should be in SectionsSkipped
	found := false
	for _, name := range result.SectionsSkipped {
		if name == "c" {
			found = true
		}
	}
	if !found {
		t.Errorf("SectionsSkipped should contain 'c', got %v", result.SectionsSkipped)
	}
}

func TestAssembler_ExtraSections(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{Name: "base", Order: 0, Content: "Base.", Enabled: true})

	asm := NewAssembler(reg, DefaultAssemblerConfig())

	extra := Section{
		Name:    "memory",
		Order:   50,
		Content: "[Memory] User likes Go.",
		Enabled: true,
	}

	result, _ := asm.Assemble(&AssemblyContext{}, extra)

	// extra (order=50) should come after base (order=0)
	if len(result.SectionsUsed) != 2 {
		t.Fatalf("SectionsUsed = %v, want 2", result.SectionsUsed)
	}
	if result.SectionsUsed[0] != "base" || result.SectionsUsed[1] != "memory" {
		t.Errorf("SectionsUsed order = %v, want [base, memory]", result.SectionsUsed)
	}
}

func TestAssembler_Metrics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{Name: "a", Order: 0, Content: "A", Enabled: true})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	_, _ = asm.Assemble(&AssemblyContext{})
	_, _ = asm.Assemble(&AssemblyContext{})

	m := asm.Metrics()
	if m.Assemblies != 2 {
		t.Errorf("Assemblies = %d, want 2", m.Assemblies)
	}
}

// ============================================================================
// PromptStage 测试
// ============================================================================

// mockTracerProvider 用于测试的 noop TracerProvider。
func noopTP() trace.TracerProvider {
	return noop_trace.NewTracerProvider()
}

func newTestLogger() *zap.SugaredLogger {
	logger, _ := zap.NewDevelopment()
	return logger.Sugar()
}

func TestPromptStage_BasicIntegration(t *testing.T) {
	// 设置 Registry
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "identity",
		Order:   0,
		Content: "You are TestBot.",
		Enabled: true,
	})
	reg.Register(Section{
		Name:    "rules",
		Order:   100,
		Content: "Be concise.",
		Enabled: true,
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	stage := NewPromptStage("prompt", asm, DefaultPromptStageConfig(), noopTP(), newTestLogger())

	// 构建 Envelope
	env := core.NewEnvelope(core.Message{
		ID:       "msg-1",
		BotID:    "bot-1",
		Channel:  "#test",
		ChatType: "private",
		UserID:   "user-1",
		Text:     "Hello",
	})

	// 执行 Stage
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	// 验证 system.prompt 已注入
	v, ok := result.Get("system.prompt")
	if !ok {
		t.Fatal("system.prompt not set in Envelope")
	}
	prompt, ok := v.(string)
	if !ok {
		t.Fatal("system.prompt is not a string")
	}
	if prompt != "You are TestBot.\n\nBe concise." {
		t.Errorf("system.prompt = %q", prompt)
	}

	// 验证 sections_used
	if v, ok := result.Get("system.prompt.sections_used"); ok {
		sections := v.([]string)
		if len(sections) != 2 {
			t.Errorf("sections_used = %v", sections)
		}
	}
}

func TestPromptStage_InjectsMemoryContext(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "identity",
		Order:   0,
		Content: "You are TestBot.",
		Enabled: true,
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	stage := NewPromptStage("prompt", asm, DefaultPromptStageConfig(), noopTP(), newTestLogger())

	env := core.NewEnvelope(core.Message{ID: "msg-1", BotID: "bot-1"})
	// 模拟上游 MemoryStage 注入记忆上下文
	env.Set("memory.context", "[Memory] User prefers Go.")

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	prompt, _ := result.Get("system.prompt")
	p := prompt.(string)
	if p != "You are TestBot.\n\n[Memory] User prefers Go." {
		t.Errorf("system.prompt = %q", p)
	}
}

func TestPromptStage_FallbackToBotConfig(t *testing.T) {
	// 空 Registry + BotConfig 提供 system prompt
	reg := NewRegistry()
	asm := NewAssembler(reg, DefaultAssemblerConfig())
	stage := NewPromptStage("prompt", asm, DefaultPromptStageConfig(), noopTP(), newTestLogger())

	env := core.NewEnvelope(core.Message{ID: "msg-1", BotID: "bot-1"})
	// 模拟 Bot.OnBeforeProcess 注入 BotConfig（以 map 形式）
	env.Set("bot.config", map[string]any{
		"systemPrompt": "I am fallback bot.",
	})

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	prompt, ok := result.Get("system.prompt")
	if !ok {
		t.Fatal("system.prompt not set")
	}
	if prompt.(string) != "I am fallback bot." {
		t.Errorf("system.prompt = %q, want %q", prompt, "I am fallback bot.")
	}
}

func TestPromptStage_ConditionalWithChatType(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{
			Name:    "identity",
			Order:   0,
			Content: "You are GroupBot.",
			Enabled: true,
		},
		Section{
			Name:    "group_behavior",
			Order:   100,
			Content: "Only respond when @mentioned.",
			Enabled: true,
			Conditional: func(ctx *AssemblyContext) bool {
				return ctx.ChatType == "group"
			},
		},
	)

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	stage := NewPromptStage("prompt", asm, DefaultPromptStageConfig(), noopTP(), newTestLogger())

	// 群聊消息
	env := core.NewEnvelope(core.Message{ID: "msg-1", ChatType: "group"})
	result, _ := stage.Process(context.Background(), env)
	prompt := result.MustGet("system.prompt").(string)
	if prompt != "You are GroupBot.\n\nOnly respond when @mentioned." {
		t.Errorf("group prompt = %q", prompt)
	}

	// 私聊消息
	env2 := core.NewEnvelope(core.Message{ID: "msg-2", ChatType: "private"})
	result2, _ := stage.Process(context.Background(), env2)
	prompt2 := result2.MustGet("system.prompt").(string)
	if prompt2 != "You are GroupBot." {
		t.Errorf("private prompt = %q", prompt2)
	}
}

func TestPromptStage_VariablesFromEnvelopeKV(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "context_aware",
		Order:   0,
		Content: "You are chatting with {{.UserName}} in {{.ChannelName}}.",
		Enabled: true,
		Variables: []Variable{
			{Name: "UserName", Source: SourceEnvelopeKV, EnvelopeKey: "user.display_name", Default: "someone"},
			{Name: "ChannelName", Source: SourceFunc, Func: func(ctx *AssemblyContext) string {
				return ctx.Channel
			}},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	stage := NewPromptStage("prompt", asm, DefaultPromptStageConfig(), noopTP(), newTestLogger())

	env := core.NewEnvelope(core.Message{
		ID:      "msg-1",
		Channel: "#random",
	})
	env.Set("user.display_name", "Luna")

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	prompt := result.MustGet("system.prompt").(string)
	expected := "You are chatting with Luna in #random."
	if prompt != expected {
		t.Errorf("prompt = %q, want %q", prompt, expected)
	}
}

// ============================================================================
// 边界 case 测试
// ============================================================================

func TestAssembler_EmptyRegistry(t *testing.T) {
	reg := NewRegistry()
	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, err := asm.Assemble(&AssemblyContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Prompt != "" {
		t.Errorf("empty registry should produce empty prompt, got %q", result.Prompt)
	}
}

func TestAssembler_MultipleVariablesInOneSection(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "multi",
		Order:   0,
		Content: "{{.A}} + {{.B}} = {{.C}}",
		Enabled: true,
		Variables: []Variable{
			{Name: "A", Source: SourceStatic, StaticValue: "1"},
			{Name: "B", Source: SourceStatic, StaticValue: "2"},
			{Name: "C", Source: SourceFunc, Func: func(_ *AssemblyContext) string { return "3" }},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, _ := asm.Assemble(&AssemblyContext{})
	if result.Prompt != "1 + 2 = 3" {
		t.Errorf("Prompt = %q, want '1 + 2 = 3'", result.Prompt)
	}
	if result.VariablesResolved != 3 {
		t.Errorf("VariablesResolved = %d, want 3", result.VariablesResolved)
	}
}

func TestAssembler_SamePlaceholderMultipleTimes(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Section{
		Name:    "repeat",
		Order:   0,
		Content: "{{.Name}} said hello. {{.Name}} is here.",
		Enabled: true,
		Variables: []Variable{
			{Name: "Name", Source: SourceStatic, StaticValue: "Luna"},
		},
	})

	asm := NewAssembler(reg, DefaultAssemblerConfig())
	result, _ := asm.Assemble(&AssemblyContext{})
	expected := "Luna said hello. Luna is here."
	if result.Prompt != expected {
		t.Errorf("Prompt = %q, want %q", result.Prompt, expected)
	}
}

func TestAssembler_CustomSeparator(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterMany(
		Section{Name: "a", Order: 0, Content: "A", Enabled: true},
		Section{Name: "b", Order: 100, Content: "B", Enabled: true},
	)

	config := DefaultAssemblerConfig()
	config.SectionSeparator = "\n---\n"
	asm := NewAssembler(reg, config)
	result, _ := asm.Assemble(&AssemblyContext{})

	if result.Prompt != "A\n---\nB" {
		t.Errorf("Prompt = %q", result.Prompt)
	}
}
