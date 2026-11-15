// Package selfhost 暴露 thinkbot 自举（self-bootstrap）能力给容器内的 bot：
// 让 LLM 能直接调用 loader 的运维接口，形成
// 「用户反馈 → bot 改源码(/app/src) → bot 编译部署 → 失败则读日志再修再部署」的自举闭环。
//
// 设计要点：
//   - 仅当 .env 开启 loader.enabled 时才注册这些工具（自托管模式），普通部署零影响。
//   - 工具全部按「敏感工具（RiskSensitive）」分级：**默认禁止**。即便已注册，
//     也必须由管理员在「工具权限」页为对应 bot/平台显式写 allow 规则才开放
//     （与 sandbox_exec / web_search 等高危害能力一致）；LLM 看不到未授权工具，
//     调用前还有 call-time 二次复核兜底。
//   - 所有变更类接口经 config 读取 loader.token 注入 X-Loader-Token 头，token 不进入 prompt，
//     LLM 永远看不到凭据；只读工具同样走受控端点。
//   - 工具经 127.0.0.1 环回访问 loader 的 ops 端口（默认 :8090），与 bot 同容器。
//   - 源码读写工具限定在 loader.git_dir 内，做路径穿越防护，避免越权改写系统文件。
package selfhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
)

// Provider 是 loader 自举能力的动态工具提供者。
type Provider struct {
	store *config.Store
	log   *zap.SugaredLogger
}

// NewProvider 构造提供者。
func NewProvider(store *config.Store, log *zap.SugaredLogger) *Provider {
	if log == nil {
		log = zap.NewNop().Sugar()
	}
	return &Provider{store: store, log: log}
}

// Tools 按当前配置返回工具列表。loader.enabled 关闭时返回 nil（不注册任何工具）。
func (p *Provider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	if !p.loaderEnabled() {
		return nil, nil
	}
	return []llm.Tool{
		p.statusTool(),
		p.deployTool(),
		p.deployStatusTool(),
		p.deployHistoryTool(),
		p.logsTool(),
		p.readSourceTool(),
		p.writeSourceTool(),
		p.rollbackTool(),
		p.restartTool(),
	}, nil
}

// loaderEnabled 读取 .env 的 loader.enabled（与 loader 自身同一开关）。
func (p *Provider) loaderEnabled() bool {
	v, ok := p.store.Get("loader.enabled")
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// call 调用 loader ops 端点，自动注入 token（来自 config，绝不暴露给 LLM）。
// 返回响应体字节；非 2xx 时连同状态码一并返回错误。
func (p *Provider) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	addr, _ := p.store.Get("loader.ops_addr")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	token, _ := p.store.Get("loader.token")
	if token == "" {
		return nil, fmt.Errorf("loader ops endpoint token is not configured (loader.token is empty); access denied")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("X-Loader-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loader request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return data, fmt.Errorf("loader returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 运维接口工具
// ---------------------------------------------------------------------------

func (p *Provider) statusTool() llm.Tool {
	return llm.Tool{
		Name: "tb_loader_status",
		Description: "Query the self-bootstrap loader status: child process PID/version/health, supervisor restart count, " +
			"deployment readiness (whether git/go/node are ready), and last deployment result. Use before deploying to verify the environment.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			data, err := p.call(ctx, http.MethodGet, "/loader/status", nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) deployTool() llm.Tool {
	return llm.Tool{
		Name: "tb_deploy",
		Description: "Trigger a self-deployment: build the current working tree (preserving your local source edits) and switch to the new binary with a health gate. " +
			"Returns the deployment id and initial status; deployment is asynchronous, so poll progress and logs with tb_deploy_status. " +
			"pull=true runs git pull --ff-only first to merge upstream; non-empty ref deploys a specific revision (discarding local edits). " +
			"Typical self-bootstrap loop: edit source -> call tb_deploy -> poll tb_deploy_status -> if failed, inspect errors with tb_logs -> fix -> redeploy.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref": map[string]any{
					"type":        "string",
					"description": "Git ref (branch/commit) to deploy. Leave empty to build the current working tree and preserve local edits.",
				},
				"pull": map[string]any{
					"type":        "boolean",
					"description": "When true, run git pull --ff-only first to merge upstream (without discarding uncommitted local changes). Default false.",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, _ := input.(map[string]any)
			body := map[string]any{}
			if args != nil {
				if v, ok := args["ref"].(string); ok && v != "" {
					body["ref"] = v
				}
				if v, ok := args["pull"].(bool); ok && v {
					body["pull"] = true
				}
			}
			data, err := p.call(ctx, http.MethodPost, "/loader/deploy", body)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) deployStatusTool() llm.Tool {
	return llm.Tool{
		Name: "tb_deploy_status",
		Description: "Query the progress and full logs (including build errors or smoke-test failure reasons) of a single self-deployment by deployment id. " +
			"status=ok means succeeded; when failed, the error and log fields show the root cause so you can fix and redeploy.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "Deployment id returned by tb_deploy",
				},
			},
			"required": []string{"id"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tb_deploy_status: invalid input")
			}
			id, _ := args["id"].(string)
			if id == "" {
				return nil, fmt.Errorf("tb_deploy_status: id is required")
			}
			data, err := p.call(ctx, http.MethodGet, "/loader/deploy/"+id, nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) deployHistoryTool() llm.Tool {
	return llm.Tool{
		Name: "tb_deploy_history",
		Description: "List audit summaries of recent deployments (id/status/start-end time/version/error) in reverse chronological order. " +
			"Use this to quickly find the id of the last failed deployment, then inspect details via tb_deploy_status or tb_logs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "Number of recent entries to return (default 10)",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, _ := input.(map[string]any)
			path := "/loader/deploy/history"
			if args != nil {
				if n, ok := args["limit"].(float64); ok && int(n) > 0 {
					path += fmt.Sprintf("?limit=%d", int(n))
				}
			}
			data, err := p.call(ctx, http.MethodGet, path, nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) logsTool() llm.Tool {
	return llm.Tool{
		Name: "tb_logs",
		Description: "Read thinkbot runtime logs or build logs from a specific deployment. " +
			"file=console (default) or json reads runtime logs; file=deploy with id reads the full build/failure log of that deployment (for self-bootstrap debugging). " +
			"lines controls how many trailing lines to return (default 200).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{
					"type":        "string",
					"description": "console=runtime console log (default), json=structured JSON log, deploy=specific deployment log (requires id)",
				},
				"id": map[string]any{
					"type":        "string",
					"description": "Deployment id when file=deploy",
				},
				"lines": map[string]any{
					"type":        "integer",
					"description": "Number of trailing lines to return (default 200)",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, _ := input.(map[string]any)
			q := []string{}
			if args != nil {
				if v, ok := args["file"].(string); ok && v != "" {
					q = append(q, "file="+v)
				}
				if v, ok := args["id"].(string); ok && v != "" {
					q = append(q, "id="+v)
				}
				if v, ok := args["lines"].(float64); ok && int(v) > 0 {
					q = append(q, fmt.Sprintf("lines=%d", int(v)))
				}
			}
			path := "/loader/logs"
			if len(q) > 0 {
				path += "?" + strings.Join(q, "&")
			}
			data, err := p.call(ctx, http.MethodGet, path, nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) rollbackTool() llm.Tool {
	return llm.Tool{
		Name:        "tb_rollback",
		Description: "Roll back to the previous known-good binary snapshot (thinkbot.prev) and restart; use for emergency recovery after a broken deployment.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			data, err := p.call(ctx, http.MethodPost, "/loader/rollback", nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}

func (p *Provider) restartTool() llm.Tool {
	return llm.Tool{
		Name: "tb_restart",
		Description: "Restart only the thinkbot child process without rebuilding; use for config reload or hung-process recovery. " +
			"Note: briefly interrupts the active service.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			data, err := p.call(ctx, http.MethodPost, "/loader/restart", nil)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		}),
	}
}
