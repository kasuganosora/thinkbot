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
		return nil, fmt.Errorf("loader 运维接口未配置 token（loader.token 为空），拒绝访问")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("X-Loader-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 loader 失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return data, fmt.Errorf("loader 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 运维接口工具
// ---------------------------------------------------------------------------

func (p *Provider) statusTool() llm.Tool {
	return llm.Tool{
		Name: "tb_loader_status",
		Description: "查询自举 loader 的运行状态：子进程 pid/版本/健康、监管重启次数、" +
			"部署能力（git/go/node 是否就绪）、最近一次部署结果。用于部署前确认环境。",
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
		Description: "触发一次自部署：编译当前工作树（保留你刚改的源码）并用健康门控切换到新二进制。" +
			"返回部署 id 与初始状态，部署是异步的，需用 tb_deploy_status 轮询进度与日志。" +
			"pull=true 会先 git pull --ff-only 合入上游；ref 非空则部署指定版本（丢弃本地改动）。" +
			"典型自举闭环：改源码 → 调本工具 → 轮询 tb_deploy_status → 若 failed 用 tb_logs 看错误 → 修复 → 再调本工具。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref": map[string]any{
					"type":        "string",
					"description": "要部署的 git ref（分支/commit）。留空=编译当前工作树，保留本地改动。",
				},
				"pull": map[string]any{
					"type":        "boolean",
					"description": "true 时先 git pull --ff-only 合入上游（不丢弃本地未提交改动）。默认 false。",
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
		Description: "按部署 id 查询单次自部署的进度与完整日志（含编译错误/smoke 失败原因）。" +
			"status=ok 表示成功，failed 时 error 与 log 字段即为失败原因，用于定位后再修复。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "tb_deploy 返回的部署 id",
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
				return nil, fmt.Errorf("tb_deploy_status: id 必填")
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
		Description: "列出最近若干次部署的审计摘要（id/状态/起止时间/版本/错误），按时间倒序。" +
			"用于快速找到“上次失败的那次”部署 id，再调 tb_deploy_status/tb_logs 看详情。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "返回最近几条，默认 10",
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
		Description: "读取 thinkbot 运行日志或某次部署的构建日志。" +
			"file=console(默认)/json 读运行日志；file=deploy 且提供 id 读该次部署的完整构建/失败日志（自举闭环排查用）。" +
			"lines 控制返回行数（默认 200）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{
					"type":        "string",
					"description": "console=运行控制台日志（默认），json=JSON 结构化日志，deploy=某次部署日志（需 id）",
				},
				"id": map[string]any{
					"type":        "string",
					"description": "file=deploy 时的部署 id",
				},
				"lines": map[string]any{
					"type":        "integer",
					"description": "返回最后多少行，默认 200",
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
		Description: "回退到上一良版本快照（thinkbot.prev）并重启，用于部署后出现严重问题时紧急恢复。",
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
		Description: "仅重启 thinkbot 子进程（不重新编译），用于配置热加载或进程卡死恢复。" +
			"注意：会短暂中断当前服务。",
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
