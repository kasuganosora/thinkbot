package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"go.uber.org/zap"
)

// ============================================================================
// Client — 单个 MCP 服务器的客户端
// ============================================================================

// Client 管理与单个 MCP 服务器的连接。
// 生命周期：Initialize → ListTools / CallTool → Close
type Client struct {
	name      string // 服务器名称（用于日志和工具名前缀）
	transport transport
	logger    *zap.SugaredLogger
	nextID    atomic.Int64
	closed    atomic.Bool
}

// newClient 创建一个 MCP 客户端（不自动初始化）。
func newClient(name string, tp transport, logger *zap.SugaredLogger) *Client {
	c := &Client{
		name:      name,
		transport: tp,
		logger:    logger.With("mcp_server", name),
	}
	return c
}

// Initialize 完成 MCP 握手。
// 必须在 ListTools / CallTool 之前调用。
func (c *Client) Initialize(ctx context.Context) (*serverInfo, error) {
	params := initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo: clientInfo{
			Name:    "thinkbot",
			Version: "1.0.0",
		},
	}

	resp, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, errs.Wrapf(err, "mcp: initialize server %q", c.name)
	}

	var result initializeResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, errs.Wrap(err, "mcp: parse initialize result")
	}

	// 发送 initialized 通知（不需要响应）
	_ = c.notify(ctx, "notifications/initialized", nil)

	return &result.ServerInfo, nil
}

// ListTools 列出服务器提供的所有工具。
func (c *Client) ListTools(ctx context.Context) ([]mcpTool, error) {
	var allTools []mcpTool
	cursor := ""
	for {
		resp, err := c.call(ctx, "tools/list", listToolsParams{Cursor: cursor})
		if err != nil {
			return nil, errs.Wrapf(err, "mcp: list tools from %q", c.name)
		}

		var result listToolsResult
		if err := json.Unmarshal(resp, &result); err != nil {
			return nil, errs.Wrap(err, "mcp: parse tools/list result")
		}

		allTools = append(allTools, result.Tools...)

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return allTools, nil
}

// CallTool 调用服务器上的一个工具。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	resp, err := c.call(ctx, "tools/call", callToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", errs.Wrapf(err, "mcp: call tool %q on %q", name, c.name)
	}

	var result callToolResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", errs.Wrap(err, "mcp: parse tools/call result")
	}

	if result.IsError {
		texts := extractText(result.Content)
		return "", fmt.Errorf("mcp: tool %q returned error: %s", name, texts)
	}

	return extractText(result.Content), nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.transport.Close()
}

// Name 返回服务器名称。
func (c *Client) Name() string { return c.name }

// ============================================================================
// JSON-RPC 内部方法
// ============================================================================

// call 发送一个 JSON-RPC 请求并等待响应。
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, errs.Wrap(err, "mcp: marshal params")
		}
		paramsRaw = b
	}

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, errs.Wrap(err, "mcp: marshal request")
	}

	respData, err := c.transport.RoundTrip(ctx, data)
	if err != nil {
		return nil, err
	}

	var resp rpcResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("mcp: parse response: %w (raw: %s)", err, string(respData))
	}

	if resp.Error != nil {
		return nil, resp.Error
	}

	return resp.Result, nil
}

// notify 发送一个 JSON-RPC 通知（无 ID，无响应）。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		paramsRaw = b
	}

	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	_, err = c.transport.RoundTrip(ctx, data)
	return err
}

// ============================================================================
// 辅助函数
// ============================================================================

// extractText 从内容块列表中拼接所有文本。
func extractText(blocks []contentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		return blocks[0].Text
	}
	var buf []byte
	for _, b := range blocks {
		if b.Text != "" {
			buf = append(buf, b.Text...)
			buf = append(buf, '\n')
		}
	}
	return string(buf[:len(buf)-1]) // 去掉最后一个换行
}

// mcpToolToLLM 将 MCP 工具定义转换为 thinkbot 的 llm.Tool。
// owner 用于创建工具执行闭包。
// toolPrefix 用于给工具名添加前缀以避免命名冲突。
func mcpToolToLLM(tool mcpTool, owner *Client, toolPrefix string) llm.Tool {
	name := tool.Name
	if toolPrefix != "" {
		name = toolPrefix + "__" + tool.Name
	}

	// 解析 inputSchema，默认空对象
	params := any(map[string]any{"type": "object"})
	if len(tool.InputSchema) > 0 {
		var p map[string]any
		if err := json.Unmarshal(tool.InputSchema, &p); err == nil {
			params = p
		}
	}

	return llm.Tool{
		Name:        name,
		Description: tool.Description,
		Parameters:  params,
		Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, _ := input.(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			result, err := owner.CallTool(ctx, tool.Name, args)
			if err != nil {
				return nil, err
			}
			return result, nil
		},
	}
}
