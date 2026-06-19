package config

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ============================================================================
// Helpers
// ============================================================================

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Setting{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func setEnv(t *testing.T, key, val string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	os.Setenv(key, val)
	t.Cleanup(func() {
		if ok {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// ============================================================================
// ParseEnvFile
// ============================================================================

func TestParseEnvFile_Basic(t *testing.T) {
	content := "# Comment line\nAPI_KEY=sk-12345\nBASE_URL=https://api.example.com\nEMPTY=\nSPACED = spaced_value\n"
	m := ParseEnvFile(content)
	if m["API_KEY"] != "sk-12345" {
		t.Errorf("API_KEY: got %q", m["API_KEY"])
	}
	if m["BASE_URL"] != "https://api.example.com" {
		t.Errorf("BASE_URL: got %q", m["BASE_URL"])
	}
	if m["EMPTY"] != "" {
		t.Errorf("EMPTY: got %q", m["EMPTY"])
	}
	if m["SPACED"] != "spaced_value" {
		t.Errorf("SPACED: got %q", m["SPACED"])
	}
}

func TestParseEnvFile_ExportPrefix(t *testing.T) {
	m := ParseEnvFile("export FOO=bar\nexport BAZ=qux\n")
	if m["FOO"] != "bar" {
		t.Errorf("FOO: got %q", m["FOO"])
	}
	if m["BAZ"] != "qux" {
		t.Errorf("BAZ: got %q", m["BAZ"])
	}
}

func TestParseEnvFile_Quotes(t *testing.T) {
	m := ParseEnvFile("DOUBLE=\"hello world\"\nSINGLE='literal value'\nESCAPED=\"line\\nbreak\"\n")
	if m["DOUBLE"] != "hello world" {
		t.Errorf("DOUBLE: got %q", m["DOUBLE"])
	}
	if m["SINGLE"] != "literal value" {
		t.Errorf("SINGLE: got %q", m["SINGLE"])
	}
	if m["ESCAPED"] != "line\nbreak" {
		t.Errorf("ESCAPED: got %q", m["ESCAPED"])
	}
}

func TestParseEnvFile_InlineComment(t *testing.T) {
	m := ParseEnvFile("KEY=value # comment\nQUOTED=\"val # not comment\" # real comment\n")
	if m["KEY"] != "value" {
		t.Errorf("KEY: got %q", m["KEY"])
	}
	if m["QUOTED"] != "val # not comment" {
		t.Errorf("QUOTED: got %q", m["QUOTED"])
	}
}

func TestLoadEnvFile_NotExist(t *testing.T) {
	m, err := LoadEnvFile("/nonexistent/path/.env")
	if err != nil {
		t.Errorf("expected nil error for missing file, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil map for missing file")
	}
}

// ============================================================================
// Key Conversion
// ============================================================================

func TestKeyConversion(t *testing.T) {
	// 只测试不含段内下划线的键（可逆）
	tests := []struct {
		env    string
		config string
	}{
		{"DB_PATH", "db.path"},
		{"LLM_TEMPERATURE", "llm.temperature"},
		{"BOT_MODEL", "bot.model"},
		{"LOG_LEVEL", "log.level"},
	}
	for _, tt := range tests {
		got := EnvKeyToConfigKey(tt.env)
		if got != tt.config {
			t.Errorf("EnvKeyToConfigKey(%q) = %q, want %q", tt.env, got, tt.config)
		}
		got2 := ConfigKeyToEnvKey(tt.config)
		if got2 != tt.env {
			t.Errorf("ConfigKeyToEnvKey(%q) = %q, want %q", tt.config, got2, tt.env)
		}
	}
}

// ============================================================================
// Store — Priority
// ============================================================================

func TestStore_Priority_OverrideWins(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	store.Set(context.Background(), "test.key", "db_value")
	store.LoadEnvMap(map[string]string{"test.key": "env_value"})
	store.SetTemporary("test.key", "override_value")

	if got := store.GetString("test.key", ""); got != "override_value" {
		t.Errorf("expected override_value, got %q", got)
	}
}

func TestStore_Priority_EnvFileOverDB(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	store.Set(context.Background(), "test.key", "db_value")
	store.LoadEnvMap(map[string]string{"test.key": "env_value"})

	if got := store.GetString("test.key", ""); got != "env_value" {
		t.Errorf("expected env_value, got %q", got)
	}
}

func TestStore_Priority_DBOverOSEnv(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	setEnv(t, "TEST_KEY", "os_value")
	store.Set(context.Background(), "test.key", "db_value")

	if got := store.GetString("test.key", ""); got != "db_value" {
		t.Errorf("expected db_value, got %q", got)
	}
}

func TestStore_OSEnvFallback(t *testing.T) {
	store := NewStore(nil)
	setEnv(t, "FALLBACK_KEY", "os_value")

	if got := store.GetString("fallback.key", ""); got != "os_value" {
		t.Errorf("expected os_value, got %q", got)
	}
}

func TestStore_OSEnvFallback_WithUnderscoreSegment(t *testing.T) {
	store := NewStore(nil)
	setEnv(t, "LLM_OPENAI_API_KEY", "sk-env-test")

	if got := store.GetString("llm.openai.api.key", ""); got != "sk-env-test" {
		t.Errorf("expected sk-env-test, got %q", got)
	}
}

// ============================================================================
// Typed Getters
// ============================================================================

func TestStore_GetInt(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("port", "8080")
	store.SetTemporary("invalid", "abc")

	if v := store.GetInt("port", 0); v != 8080 {
		t.Errorf("port: expected 8080, got %d", v)
	}
	if v := store.GetInt("invalid", 99); v != 99 {
		t.Errorf("invalid: expected default 99, got %d", v)
	}
}

func TestStore_GetBool(t *testing.T) {
	store := NewStore(nil)
	for _, tt := range []struct{ val string; exp bool }{
		{"true", true}, {"1", true}, {"yes", true}, {"on", true},
		{"false", false}, {"0", false}, {"no", false}, {"off", false},
	} {
		store.SetTemporary("flag", tt.val)
		if v := store.GetBool("flag", !tt.exp); v != tt.exp {
			t.Errorf("GetBool(%q): expected %v, got %v", tt.val, tt.exp, v)
		}
	}
}

func TestStore_GetFloat64(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("temp", "0.75")
	if v := store.GetFloat64("temp", 0); v != 0.75 {
		t.Errorf("expected 0.75, got %f", v)
	}
}

func TestStore_GetDuration(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("t1", "30s")
	if v := store.GetDuration("t1", 0); v != 30*time.Second {
		t.Errorf("expected 30s, got %v", v)
	}
	store.SetTemporary("t2", "120")
	if v := store.GetDuration("t2", 0); v != 120*time.Second {
		t.Errorf("expected 120s, got %v", v)
	}
}

func TestStore_GetStringSlice(t *testing.T) {
	store := NewStore(nil)
	store.SetTemporary("tags", "a, b, c")
	s := store.GetStringSlice("tags", nil)
	if len(s) != 3 || s[0] != "a" || s[2] != "c" {
		t.Errorf("expected [a b c], got %v", s)
	}
}

// ============================================================================
// Persist & Reload
// ============================================================================

func TestStore_PersistAndReload(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.Set(ctx, "app.name", "thinkbot")
	store.Set(ctx, "app.version", "1.0")

	store2 := NewStore(db)
	store2.Reload(ctx)

	if v := store2.GetString("app.name", ""); v != "thinkbot" {
		t.Errorf("app.name: got %q", v)
	}
}

func TestStore_UpdateExisting(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.Set(ctx, "key", "v1")
	store.Set(ctx, "key", "v2")

	var settings []Setting
	db.Find(&settings)
	if len(settings) != 1 {
		t.Fatalf("expected 1 row, got %d", len(settings))
	}
	if settings[0].Value != "v2" {
		t.Errorf("expected v2, got %q", settings[0].Value)
	}
}

func TestStore_Delete(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.Set(ctx, "myapp.test.delete", "val")
	store.Delete(ctx, "myapp.test.delete")

	if _, ok := store.Get("myapp.test.delete"); ok {
		t.Error("key should be deleted")
	}
}

// ============================================================================
// GetByPrefix & Unmarshal
// ============================================================================

func TestStore_GetByPrefix(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"llm.openai.api_key":    "sk-xxx",
		"llm.openai.base_url":   "https://api.openai.com",
		"llm.anthropic.api_key": "sk-ant-xxx",
		"bot.system_prompt":     "hello",
	})

	result := store.GetByPrefix("llm")
	if len(result) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(result))
	}
	if result["openai.api_key"] != "sk-xxx" {
		t.Errorf("openai.api_key: got %q", result["openai.api_key"])
	}
}

func TestStore_Unmarshal(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"bot.model":       "gpt-4o",
		"bot.temperature": "0.7",
		"bot.max_tokens":  "4096",
	})

	var cfg struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}
	if err := store.Unmarshal("bot", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Model != "gpt-4o" {
		t.Errorf("Model: got %q", cfg.Model)
	}
	if cfg.Temperature != 0.7 {
		t.Errorf("Temperature: got %f", cfg.Temperature)
	}
}

// ============================================================================
// OnChange
// ============================================================================

func TestStore_OnChange(t *testing.T) {
	store := NewStore(nil)
	var changes []string
	unsub := store.OnChange(func(key, oldVal, newVal string) {
		changes = append(changes, key+":"+oldVal+"->"+newVal)
	})

	store.SetTemporary("foo", "bar")
	store.SetTemporary("foo", "baz")

	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if changes[0] != "foo:->bar" {
		t.Errorf("change[0]: got %q", changes[0])
	}

	unsub()
	store.SetTemporary("foo", "qux")
	if len(changes) != 2 {
		t.Error("no new changes after unsubscribe")
	}
}

// ============================================================================
// Builder
// ============================================================================

func TestBuilder_GetBotSettings(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"bot.model":         "gpt-4o",
		"bot.system_prompt": "hello",
	})

	b := NewBuilder(store, testLogger())
	s := b.GetBotSettings()

	if s.Model != "gpt-4o" {
		t.Errorf("Model: got %q", s.Model)
	}
	if s.Temperature != 0.7 {
		t.Errorf("Temperature default: got %f", s.Temperature)
	}
}

// ============================================================================
// ValidateKey
// ============================================================================

func TestValidateKey(t *testing.T) {
	for _, k := range []string{"llm.api_key", "db.path", "bot_1.name", "simple"} {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q): unexpected error: %v", k, err)
		}
	}
	for _, k := range []string{"", "UPPER", "key with space", "key@special"} {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q): expected error", k)
		}
	}
}

// ============================================================================
// 元数据（Category / Description）
// ============================================================================

func TestStore_SetWithMeta(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.SetWithMeta(ctx, "llm.openai.api_key", "sk-xxx", "LLM", "OpenAI API Key"); err != nil {
		t.Fatalf("SetWithMeta: %v", err)
	}

	setting, err := store.GetSetting(ctx, "llm.openai.api_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if setting.Value != "sk-xxx" {
		t.Errorf("Value: got %q", setting.Value)
	}
	if setting.Category != "LLM" {
		t.Errorf("Category: got %q", setting.Category)
	}
	if setting.Description != "OpenAI API Key" {
		t.Errorf("Description: got %q", setting.Description)
	}
}

func TestStore_SetWithMeta_OverwriteValueOnly(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.SetWithMeta(ctx, "llm.model", "gpt-4o", "LLM", "Default model")
	store.Set(ctx, "llm.model", "gpt-4o-mini")

	setting, err := store.GetSetting(ctx, "llm.model")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if setting.Value != "gpt-4o-mini" {
		t.Errorf("Value: got %q", setting.Value)
	}
	if setting.Category != "LLM" {
		t.Errorf("Category should be preserved: got %q", setting.Category)
	}
	if setting.Description != "Default model" {
		t.Errorf("Description should be preserved: got %q", setting.Description)
	}
}

func TestStore_SetWithMeta_UpdateMeta(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.SetWithMeta(ctx, "bot.model", "gpt-4o", "Bot", "Initial")
	store.SetWithMeta(ctx, "bot.model", "claude", "Bot", "Updated description")

	setting, _ := store.GetSetting(ctx, "bot.model")
	if setting.Value != "claude" {
		t.Errorf("Value: got %q", setting.Value)
	}
	if setting.Description != "Updated description" {
		t.Errorf("Description: got %q", setting.Description)
	}
}

func TestStore_RegisterMeta(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.RegisterMeta(ctx, "bot.workers", "Bot", "Worker count"); err != nil {
		t.Fatalf("RegisterMeta: %v", err)
	}

	setting, err := store.GetSetting(ctx, "bot.workers")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if setting.Category != "Bot" {
		t.Errorf("Category: got %q", setting.Category)
	}
	if setting.Description != "Worker count" {
		t.Errorf("Description: got %q", setting.Description)
	}

	store.Set(ctx, "bot.workers", "8")
	setting, _ = store.GetSetting(ctx, "bot.workers")
	if setting.Value != "8" {
		t.Errorf("Value: got %q", setting.Value)
	}
	if setting.Category != "Bot" {
		t.Errorf("Category lost after Set: got %q", setting.Category)
	}
}

func TestStore_RegisterMany(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	metas := []MetaSpec{
		{Key: "llm.provider", Category: "LLM", Description: "AI provider name"},
		{Key: "llm.model", Category: "LLM", Description: "Model identifier"},
		{Key: "bot.workers", Category: "Bot", Description: "Worker count"},
	}
	if err := store.RegisterMany(ctx, metas); err != nil {
		t.Fatalf("RegisterMany: %v", err)
	}

	setting, _ := store.GetSetting(ctx, "llm.model")
	if setting.Description != "Model identifier" {
		t.Errorf("llm.model description: got %q", setting.Description)
	}
}

func TestStore_ListSettings(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.SetWithMeta(ctx, "llm.api_key", "sk-1", "LLM", "API Key")
	store.SetWithMeta(ctx, "bot.model", "gpt-4o", "Bot", "Model")
	store.Set(ctx, "db.path", "data.db")

	settings, err := store.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if len(settings) != 3 {
		t.Fatalf("expected 3 settings, got %d", len(settings))
	}
	if settings[0].Category != "" {
		t.Errorf("first category should be empty, got %q", settings[0].Category)
	}
	if settings[1].Category != "Bot" {
		t.Errorf("second category should be Bot, got %q", settings[1].Category)
	}
}

func TestStore_ListByCategory(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.SetWithMeta(ctx, "llm.api_key", "sk-1", "LLM", "API Key")
	store.SetWithMeta(ctx, "llm.model", "gpt-4o", "LLM", "Model")
	store.SetWithMeta(ctx, "bot.workers", "4", "Bot", "Workers")

	llmSettings, err := store.ListByCategory(ctx, "LLM")
	if err != nil {
		t.Fatalf("ListByCategory: %v", err)
	}
	if len(llmSettings) != 2 {
		t.Fatalf("expected 2 LLM settings, got %d", len(llmSettings))
	}
}

func TestStore_ListCategories(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.SetWithMeta(ctx, "llm.api_key", "sk-1", "LLM", "API Key")
	store.SetWithMeta(ctx, "bot.workers", "4", "Bot", "Workers")
	store.Set(ctx, "db.path", "data.db")

	categories, err := store.ListCategories(ctx)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(categories) != 2 {
		t.Fatalf("expected 2 categories, got %d: %v", len(categories), categories)
	}
}

// ============================================================================
// 多 LLM 模型
// ============================================================================

func TestBuilder_GetLLMModel(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"llm.main":  `{"provider":"openai","model":"gpt-4o","api_key":"sk-main","temperature":0.7,"max_tokens":4096}`,
		"llm.light": `{"provider":"openai","model":"gpt-4o-mini","api_key":"sk-light","temperature":0.3}`,
	})

	b := NewBuilder(store, testLogger())

	// 单个读取
	main, ok := b.GetLLMModel("main")
	if !ok {
		t.Fatal("model 'main' not found")
	}
	if main.Provider != "openai" || main.Model != "gpt-4o" || main.APIKey != "sk-main" {
		t.Errorf("main: %+v", main)
	}
	if main.Temperature != 0.7 || main.MaxTokens != 4096 {
		t.Errorf("main defaults: temp=%f max=%d", main.Temperature, main.MaxTokens)
	}

	// 默认值填充
	light, _ := b.GetLLMModel("light")
	if light.MaxTokens != 4096 {
		t.Errorf("light max_tokens default: got %d", light.MaxTokens)
	}

	// 不存在
	_, ok = b.GetLLMModel("nonexistent")
	if ok {
		t.Error("should not find nonexistent")
	}
}

func TestBuilder_GetAllLLMModels(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"llm.main":   `{"provider":"openai","model":"gpt-4o","api_key":"sk-1"}`,
		"llm.light":  `{"provider":"openai","model":"gpt-4o-mini","api_key":"sk-2"}`,
		"llm.claude": `{"provider":"anthropic","model":"claude-sonnet-4-20250514","api_key":"sk-3"}`,
	})

	b := NewBuilder(store, testLogger())
	models := b.GetAllLLMModels()

	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if _, ok := models["main"]; !ok {
		t.Error("missing 'main'")
	}
	if _, ok := models["claude"]; !ok {
		t.Error("missing 'claude'")
	}
}

func TestBuilder_GetBotLLMAssignment(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"bot.mybot.main":  "main",
		"bot.mybot.light": "light",
	})

	b := NewBuilder(store, testLogger())
	a := b.GetBotLLMAssignment("mybot")

	if a.Main != "main" {
		t.Errorf("main: got %q", a.Main)
	}
	if a.Light != "light" {
		t.Errorf("light: got %q", a.Light)
	}
}

func TestBuilder_GetBotLLMAssignment_LightFallback(t *testing.T) {
	store := NewStore(nil)
	store.LoadEnvMap(map[string]string{
		"bot.mybot.main": "main",
	})

	b := NewBuilder(store, testLogger())
	a := b.GetBotLLMAssignment("mybot")

	if a.Main != "main" {
		t.Errorf("main: got %q", a.Main)
	}
	if a.Light != "main" {
		t.Errorf("light should fallback to main: got %q", a.Light)
	}
}

func TestBuilder_GetBotLLMAssignment_Empty(t *testing.T) {
	store := NewStore(nil)
	b := NewBuilder(store, testLogger())
	a := b.GetBotLLMAssignment("mybot")

	if a.Main != "" {
		t.Errorf("main should be empty: got %q", a.Main)
	}
}
