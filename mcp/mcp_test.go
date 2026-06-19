package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// mockTransport — 测试用传输层
// ============================================================================

type mockTransport struct {
	mu       sync.Mutex
	handlers map[string]func(json.RawMessage) (json.RawMessage, error)
	calls    []string
	closed   bool
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		handlers: make(map[string]func(json.RawMessage) (json.RawMessage, error)),
	}
}

func (t *mockTransport) RoundTrip(_ context.Context, data []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	t.calls = append(t.calls, req.Method)

	// 通知（无 ID）返回空响应
	if req.ID == nil {
		return []byte(`{}`), nil
	}

	handler, ok := t.handlers[req.Method]
	if !ok {
		resp := rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    -32601,
				Message: "method not found",
			},
		}
		b, _ := json.Marshal(resp)
		return b, nil
	}

	result, err := handler(req.Params)
	if err != nil {
		resp := rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    -1,
				Message: err.Error(),
			},
		}
		b, _ := json.Marshal(resp)
		return b, nil
	}

	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
	b, _ := json.Marshal(resp)
	return b, nil
}

func (t *mockTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func (t *mockTransport) wasCalled(method string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.calls {
		if c == method {
			return true
		}
	}
	return false
}

// ============================================================================
// 辅助：创建一个带标准 MCP 响应的 mockTransport
// ============================================================================

func setupMockTransport(tools []mcpTool, callResult string) *mockTransport {
	tp := newMockTransport()

	tp.handlers["initialize"] = func(_ json.RawMessage) (json.RawMessage, error) {
		result := initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{},
			ServerInfo:      serverInfo{Name: "test-server", Version: "1.0"},
		}
		b, _ := json.Marshal(result)
		return b, nil
	}

	tp.handlers["tools/list"] = func(_ json.RawMessage) (json.RawMessage, error) {
		result := listToolsResult{Tools: tools}
		b, _ := json.Marshal(result)
		return b, nil
	}

	tp.handlers["tools/call"] = func(params json.RawMessage) (json.RawMessage, error) {
		_ = params // 忽略参数细节
		result := callToolResult{
			Content: []contentBlock{{Type: "text", Text: callResult}},
		}
		b, _ := json.Marshal(result)
		return b, nil
	}

	return tp
}

// ============================================================================
// Client 测试
// ============================================================================

func TestClient_Initialize(t *testing.T) {
	tp := setupMockTransport(nil, "")
	client := newClient("test", tp, zap.NewNop().Sugar())

	info, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "test-server" {
		t.Errorf("expected server name 'test-server', got %q", info.Name)
	}
	if !tp.wasCalled("initialize") {
		t.Error("expected initialize method to be called")
	}
	// initialized 通知也应被发送
	if !tp.wasCalled("notifications/initialized") {
		t.Error("expected notifications/initialized to be sent")
	}
}

func TestClient_ListTools(t *testing.T) {
	mockTools := []mcpTool{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
		{
			Name:        "write_file",
			Description: "Write a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
		},
	}
	tp := setupMockTransport(mockTools, "")
	client := newClient("test", tp, zap.NewNop().Sugar())
	_, _ = client.Initialize(context.Background())

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Errorf("expected tool[0] name 'read_file', got %q", tools[0].Name)
	}
}

func TestClient_CallTool(t *testing.T) {
	tp := setupMockTransport(nil, "file content: hello")
	client := newClient("test", tp, zap.NewNop().Sugar())
	_, _ = client.Initialize(context.Background())

	result, err := client.CallTool(context.Background(), "read_file", map[string]any{"path": "/tmp/test.txt"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "file content: hello" {
		t.Errorf("expected 'file content: hello', got %q", result)
	}
}

func TestClient_CallTool_Error(t *testing.T) {
	tp := newMockTransport()
	tp.handlers["tools/call"] = func(_ json.RawMessage) (json.RawMessage, error) {
		result := callToolResult{
			Content: []contentBlock{{Type: "text", Text: "file not found"}},
			IsError: true,
		}
		b, _ := json.Marshal(result)
		return b, nil
	}
	client := newClient("test", tp, zap.NewNop().Sugar())

	_, err := client.CallTool(context.Background(), "read_file", nil)
	if err == nil {
		t.Fatal("expected error for is_error response")
	}
}

func TestClient_Close(t *testing.T) {
	tp := setupMockTransport(nil, "")
	client := newClient("test", tp, zap.NewNop().Sugar())

	_ = client.Close()
	if !tp.closed {
		t.Error("transport should be closed")
	}

	// Close again should be no-op
	_ = client.Close()
}

// ============================================================================
// mcpToolToLLM 测试
// ============================================================================

func TestMcpToolToLLM(t *testing.T) {
	tp := setupMockTransport(nil, "result text")
	client := newClient("myserver", tp, zap.NewNop().Sugar())

	tool := mcpTool{
		Name:        "search",
		Description: "Search the web",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}

	llmTool := mcpToolToLLM(tool, client, "myserver")

	if llmTool.Name != "myserver__search" {
		t.Errorf("expected name 'myserver__search', got %q", llmTool.Name)
	}
	if llmTool.Description != "Search the web" {
		t.Errorf("description mismatch: %q", llmTool.Description)
	}

	// 验证 Execute 函数
	if llmTool.Execute == nil {
		t.Fatal("Execute should not be nil")
	}

	result, err := llmTool.Execute(&llm.ToolExecContext{}, map[string]any{"query": "golang"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "result text" {
		t.Errorf("expected 'result text', got %v", result)
	}
}

// ============================================================================
// Manager 测试
// ============================================================================

func TestManager_ConnectAndListTools(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport([]mcpTool{
		{Name: "tool_a", Description: "Tool A"},
		{Name: "tool_b", Description: "Tool B"},
	}, "ok")

	// 直接注入 mock transport 到 client
	client := newClient("server1", tp, zap.NewNop().Sugar())
	info, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_ = info

	mgr.mu.Lock()
	mgr.clients["server1"] = client
	mgr.configs["server1"] = ServerConfig{Name: "server1", Enabled: true}
	mgr.mu.Unlock()

	servers := mgr.ConnectedServers()
	if len(servers) != 1 || servers[0] != "server1" {
		t.Errorf("expected [server1], got %v", servers)
	}

	allTools, err := mgr.ListAllTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	if len(allTools["server1"]) != 2 {
		t.Errorf("expected 2 tools, got %d", len(allTools["server1"]))
	}
}

func TestManager_Close(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport(nil, "")
	client := newClient("srv", tp, zap.NewNop().Sugar())
	mgr.mu.Lock()
	mgr.clients["srv"] = client
	mgr.mu.Unlock()

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !tp.closed {
		t.Error("transport should be closed")
	}
	if len(mgr.ConnectedServers()) != 0 {
		t.Error("no servers should remain after close")
	}
}

// ============================================================================
// Provider 测试
// ============================================================================

func TestProvider_Tools(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport([]mcpTool{
		{Name: "get_weather", Description: "Get weather info"},
	}, "sunny 25C")

	client := newClient("weather_srv", tp, zap.NewNop().Sugar())
	_, _ = client.Initialize(context.Background())

	mgr.mu.Lock()
	mgr.clients["weather_srv"] = client
	mgr.mu.Unlock()

	provider := NewProvider(mgr)
	sctx := &tools.ToolSessionContext{}

	result, err := provider.Tools(context.Background(), sctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Name != "weather_srv__get_weather" {
		t.Errorf("expected 'weather_srv__get_weather', got %q", result[0].Name)
	}

	// 第二次调用应该用缓存（不会额外调用 tools/list）
	result2, _ := provider.Tools(context.Background(), sctx)
	if len(result2) != 1 {
		t.Errorf("cached result should have 1 tool")
	}
}

func TestProvider_SubagentFiltered(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport([]mcpTool{
		{Name: "tool1", Description: "T1"},
	}, "ok")
	client := newClient("srv", tp, zap.NewNop().Sugar())
	mgr.mu.Lock()
	mgr.clients["srv"] = client
	mgr.mu.Unlock()

	provider := NewProvider(mgr)
	sctx := &tools.ToolSessionContext{IsSubagent: true}

	result, err := provider.Tools(context.Background(), sctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil for subagent context, got %d tools", len(result))
	}
}

func TestProvider_InvalidateCache(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport([]mcpTool{
		{Name: "tool1", Description: "T1"},
	}, "ok")
	client := newClient("srv", tp, zap.NewNop().Sugar())
	mgr.mu.Lock()
	mgr.clients["srv"] = client
	mgr.mu.Unlock()

	provider := NewProvider(mgr)
	sctx := &tools.ToolSessionContext{}

	// 首次获取
	r1, _ := provider.Tools(context.Background(), sctx)
	if len(r1) != 1 {
		t.Fatal("expected 1 tool")
	}

	// 使缓存失效
	provider.InvalidateCache()

	// 再次获取应该重新请求
	r2, _ := provider.Tools(context.Background(), sctx)
	if len(r2) != 1 {
		t.Fatal("expected 1 tool after cache invalidation")
	}
}

// ============================================================================
// Manager Enable/Disable 测试
// ============================================================================

func TestManager_EnableDisable(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	// 注册一个未启用的服务器
	mgr.AddServer(ServerConfig{
		Name:      "srv",
		Transport: "stdio",
		Command:   "echo",
		Enabled:   false,
	})

	// 初始状态
	if mgr.IsServerEnabled("srv") {
		t.Error("server should be disabled initially")
	}
	if mgr.IsServerConnected("srv") {
		t.Error("server should not be connected initially")
	}

	// ListServers 应反映状态
	statuses := mgr.ListServers()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 server status, got %d", len(statuses))
	}
	if statuses[0].Enabled {
		t.Error("ListServers should show Enabled=false")
	}

	// DisableServer 对未启用的服务器是幂等的
	if err := mgr.DisableServer("srv"); err != nil {
		t.Fatalf("DisableServer on disabled: %v", err)
	}

	// EnableServer 不存在的服务器应报错
	if err := mgr.EnableServer(context.Background(), "nonexistent"); err == nil {
		t.Error("EnableServer on nonexistent should error")
	}
}

func TestManager_DisableServer_NotConfigured(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())
	if err := mgr.DisableServer("nonexistent"); err == nil {
		t.Error("DisableServer on nonexistent should error")
	}
}

func TestManager_ServerChangeCallback(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	called := 0
	mgr.SetOnServerChange(func() {
		called++
	})

	mgr.AddServer(ServerConfig{Name: "srv", Enabled: true})
	// AddServer 不触发回调（只是配置，不改变连接状态）

	// 手动注入一个 client 模拟已连接状态
	tp := setupMockTransport(nil, "")
	client := newClient("srv", tp, zap.NewNop().Sugar())
	mgr.mu.Lock()
	mgr.clients["srv"] = client
	mgr.configs["srv"] = ServerConfig{Name: "srv", Enabled: true}
	mgr.mu.Unlock()

	// DisableServer 应触发回调并断开
	if err := mgr.DisableServer("srv"); err != nil {
		t.Fatalf("DisableServer: %v", err)
	}
	if called != 1 {
		t.Errorf("callback should be called once, got %d", called)
	}
	if !tp.closed {
		t.Error("transport should be closed after DisableServer")
	}
	if mgr.IsServerConnected("srv") {
		t.Error("server should be disconnected after DisableServer")
	}
}

// ============================================================================
// RegisterTools 集成测试
// ============================================================================

func TestRegisterTools(t *testing.T) {
	mgr := NewManager(zap.NewNop().Sugar())

	tp := setupMockTransport([]mcpTool{
		{Name: "tool_x", Description: "Tool X"},
	}, "ok")
	client := newClient("srv_x", tp, zap.NewNop().Sugar())
	_, _ = client.Initialize(context.Background())
	mgr.mu.Lock()
	mgr.clients["srv_x"] = client
	mgr.mu.Unlock()

	toolMgr := tools.NewToolManager(prompt.NewRegistry(), zap.NewNop().Sugar())

	if err := RegisterTools(toolMgr, mgr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	// 验证 Provider 已注册
	sctx := &tools.ToolSessionContext{}
	resolved, err := toolMgr.ResolveTools(context.Background(), sctx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved tool, got %d", len(resolved))
	}
	if resolved[0].Name != "srv_x__tool_x" {
		t.Errorf("expected 'srv_x__tool_x', got %q", resolved[0].Name)
	}
}

func TestRegisterTools_NilManager(t *testing.T) {
	toolMgr := tools.NewToolManager(prompt.NewRegistry(), zap.NewNop().Sugar())
	if err := RegisterTools(toolMgr, nil); err != nil {
		t.Fatalf("RegisterTools with nil manager should not error: %v", err)
	}
}

// ============================================================================
// extractText 测试
// ============================================================================

func TestExtractText(t *testing.T) {
	tests := []struct {
		name   string
		blocks []contentBlock
		want   string
	}{
		{"empty", nil, ""},
		{"single", []contentBlock{{Type: "text", Text: "hello"}}, "hello"},
		{"multi", []contentBlock{
			{Type: "text", Text: "line1"},
			{Type: "text", Text: "line2"},
		}, "line1\nline2"},
		{"with_empty", []contentBlock{
			{Type: "text", Text: "a"},
			{Type: "image"}, // no text
			{Type: "text", Text: "b"},
		}, "a\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractText(tt.blocks)
			if got != tt.want {
				t.Errorf("extractText() = %q, want %q", got, tt.want)
			}
		})
	}
}
