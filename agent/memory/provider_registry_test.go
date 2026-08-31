package memory

import (
	"context"
	"testing"
)

// MockMemoryProvider 用于测试的 mock provider。
type MockMemoryProvider struct {
	name      string
	available bool
}

func (m *MockMemoryProvider) Name() string      { return m.name }
func (m *MockMemoryProvider) IsAvailable() bool { return m.available }
func (m *MockMemoryProvider) Initialize(_ context.Context, _ string, _ ...ProviderOption) error {
	return nil
}
func (m *MockMemoryProvider) SystemPromptBlock() string { return "" }
func (m *MockMemoryProvider) Prefetch(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *MockMemoryProvider) QueuePrefetch(_ context.Context, _, _ string)     {}
func (m *MockMemoryProvider) SyncTurn(_ context.Context, _, _, _ string) error { return nil }
func (m *MockMemoryProvider) OnSessionEnd(_ context.Context, _ string)         {}
func (m *MockMemoryProvider) Shutdown() error                                  { return nil }

func TestProviderRegistry_RegisterAndCreate(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	// 注册工厂
	mockFactory := func(config ProviderFactoryConfig) (MemoryProvider, error) {
		return &MockMemoryProvider{name: config.Name, available: true}, nil
	}
	reg.Register("mock", ProviderEntry{
		Factory:        mockFactory,
		DefaultEnabled: true,
		Priority:       10,
	})

	// 验证注册
	if !reg.IsRegistered("mock") {
		t.Fatal("expected mock to be registered")
	}

	names := reg.Names()
	if len(names) != 1 || names[0] != "mock" {
		t.Errorf("expected names=[mock], got %v", names)
	}

	// 创建
	provider, err := reg.Create("mock", ProviderFactoryConfig{
		Name:    "mock",
		HomeDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if provider.Name() != "mock" {
		t.Errorf("expected name=mock, got %s", provider.Name())
	}
}

func TestProviderRegistry_CacheHit(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	callCount := 0
	mockFactory := func(config ProviderFactoryConfig) (MemoryProvider, error) {
		callCount++
		return &MockMemoryProvider{name: config.Name, available: true}, nil
	}
	reg.Register("mock", ProviderEntry{Factory: mockFactory})

	// 相同配置创建两次，工厂应只调用一次
	cfg := ProviderFactoryConfig{
		Name:     "mock",
		Platform: "cli",
		BotID:    "bot1",
		UserID:   "user1",
	}

	p1, err := reg.Create("mock", cfg)
	if err != nil {
		t.Fatal(err)
	}

	p2, err := reg.Create("mock", cfg)
	if err != nil {
		t.Fatal(err)
	}

	if callCount != 1 {
		t.Errorf("expected factory called once, got %d", callCount)
	}

	if p1 != p2 {
		t.Error("expected same instance from cache")
	}
}

func TestProviderRegistry_UnknownProvider(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	_, err := reg.Create("nonexistent", ProviderFactoryConfig{})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestProviderRegistry_DefaultEnabled(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	reg.Register("builtin", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "builtin", available: true}, nil
		},
		DefaultEnabled: true,
		Priority:       100,
	})

	reg.Register("optional", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "optional", available: true}, nil
		},
		DefaultEnabled: false,
		Priority:       10,
	})

	providers := reg.CreateDefaultEnabled(ProviderFactoryConfig{})

	if len(providers) != 1 {
		t.Fatalf("expected 1 default-enabled provider, got %d", len(providers))
	}
	if providers[0].Name() != "builtin" {
		t.Errorf("expected builtin, got %s", providers[0].Name())
	}
}

func TestProviderRegistry_BestAvailable(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	reg.Register("low", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "low", available: false}, nil
		},
		Priority: 5,
	})

	reg.Register("high", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "high", available: true}, nil
		},
		Priority: 10,
	})

	provider, err := reg.BestAvailable(ProviderFactoryConfig{})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if provider.Name() != "high" {
		t.Errorf("expected high (highest priority available), got %s", provider.Name())
	}
}

func TestProviderRegistry_Unregister(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	reg.Register("mock", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "mock", available: true}, nil
		},
	})

	if !reg.IsRegistered("mock") {
		t.Fatal("expected registered")
	}

	reg.Unregister("mock")

	if reg.IsRegistered("mock") {
		t.Fatal("expected unregistered")
	}
}

func TestProviderRegistry_ClearCache(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	callCount := 0
	reg.Register("mock", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			callCount++
			return &MockMemoryProvider{name: "mock", available: true}, nil
		},
	})

	cfg := ProviderFactoryConfig{Name: "mock", Platform: "test"}

	_, _ = reg.Create("mock", cfg)
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// 清除缓存后再创建
	reg.ClearCache()
	_, _ = reg.Create("mock", cfg)
	if callCount != 2 {
		t.Fatalf("expected 2 calls after cache clear, got %d", callCount)
	}
}

func TestProviderRegistry_HealthCheck(t *testing.T) {
	reg := NewProviderRegistry(testLogger())

	reg.Register("healthy", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "healthy", available: true}, nil
		},
	})

	reg.Register("sick", ProviderEntry{
		Factory: func(cfg ProviderFactoryConfig) (MemoryProvider, error) {
			return &MockMemoryProvider{name: "sick", available: false}, nil
		},
	})

	results := reg.HealthCheck(context.Background())

	if !results["healthy"] {
		t.Error("expected healthy=true")
	}
	if results["sick"] {
		t.Error("expected sick=false")
	}
}
