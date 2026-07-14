package pipeline

import (
	"context"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// VerificationGateMiddleware — 确定性"防偷懒"门禁（强制性工具调用）
//
// 问题场景（与 DECO 护栏层同构）：
//   - 用户问"有没有安装 git""python 版本是多少""xxx 服务在跑吗"
//   - 模型不调工具，凭训练记忆直接编造环境状态结论并交付
//
// 设计原则（来自 DECO 文章总纲「能用确定性兜底的，别交给模型」）：
//   不在 prompt 里求模型"自觉去查"，而是在框架层把"不调工具就 finalize"
//   的路径物理堵死——对环境类问题强制 tool_choice=required，模型在拿到
//   真实工具结果前无法返回最终答案。
//
// 与 LazyResponseMiddleware 的关系：
//   - 本 Middleware 是「事前强制」（当轮即阻断幻觉，确定性）
//   - LazyResponseMiddleware 是「事后兜底」（覆盖分类器漏判的变体，防御纵深）
//
// 使用方式：
//
//	wrappedLLM := pipeline.WithMiddleware(
//	    pipeline.VerificationGateMiddleware(pipeline.NewVerificationGateConfig()),
//	    llmStage,
//	)
//
// 它只负责在 Envelope 上打 `verify.required` 标记；真正的 tool_choice 强制
// 由 LLMStage 读取该标记后在 OrchestrateConfig.ToolChoiceForStep 中落地
// （见 agent/stages/llmroute.go）。
// ============================================================================

// 环境状态实体：其状态必须用工具核实，不可凭记忆猜测。
var envEntityPat = regexp.MustCompile(`(?i)(` +
	`git|python3?|pip|node|nvm|npx|npm|yarn|pnpm|go|golang|java|jdk|maven|` +
	`gcc|clang|docker|podman|kubectl|k8s|redis|mysql|postgres|postgresql|nginx|apache|` +
	`ssh|curl|wget|telnet|netcat|\bnc\b|apt|apt-get|yum|dnf|brew|rpm|dpkg|systemctl|` +
	`包管理器|依赖|软件|命令|程序|服务|进程|端口|防火墙|环境变量|` +
	`\benv\b|PATH|系统|操作系统|内核|内存|磁盘|cpu|gpu|文件|目录|文件夹|脚本|容器|镜像|sandbox` +
	`)`)

// 核实类动词：显式要求"检查 / 确认状态"的措辞。
var verifyVerbPat = regexp.MustCompile(`(?i)(` +
	`有没有|是否|装了没|安装了没|存在吗|存在么|运行吗|在运行|启动了没|` +
	`在跑|跑着|启动着|开着|被占用|占用了吗|通不通|通吗|连得上|存活|` +
	`检查|查看|确认|探测|验证|查一下|看看.*是否|有无|装没装|启用没|开启没|可用吗|能用吗|装了吗` +
	`)`)

// shell 检查惯用法：which / command -v / type / 包管理列举等。
var shellCheckPat = regexp.MustCompile(`(?i)(` +
	`\bwhich\s+\w+|\bcommand\s+-v\s+\w+|\btype\s+-?p?\s+\w+|` +
	`apt\s+list|yum\s+list|dnf\s+list|dpkg\s+-l|rpm\s+-qa|pip\s+show|pip3\s+show|npm\s+ls|go\s+version|node\s+-v|python3?\s+-V` +
	`)`)

// 环境状态名词短语：本身就是环境状态查询，无需动词。
var envStateNounPat = regexp.MustCompile(`(?i)(` +
	`系统信息|系统版本|操作系统|内核版本|内存大小|内存占用|磁盘空间|磁盘容量|` +
	`cpu\s*占用|gpu\s*占用|当前用户|当前目录|工作目录|环境变量|系统环境|运行环境|沙箱环境|` +
	`端口|端口占用|端口号|进程占用|连接数` +
	`)`)

// 实体 + 版本：git 版本 / python 的版本 等。
var entityVersionPat = regexp.MustCompile(`(?i)(` +
	`git|python3?|node|go|golang|java|jdk|npm|docker|kubectl|redis|mysql|nginx|` +
	`clang|gcc|php|ruby|rust|cargo|内核|系统|操作系统|数据库|中间件` +
	`).{0,8}(版本|version|版)`)

// requiresVerification 确定性判定用户问题是否属于"需工具核实的环境类问题"。
// 返回 true 时，框架应强制模型先调工具再作答。
func requiresVerification(text string) bool {
	t := strings.TrimSpace(text)
	if len(t) == 0 {
		return false
	}
	// 1) 本身就是环境状态查询
	if envStateNounPat.MatchString(t) {
		return true
	}
	// 2) shell 检查惯用法
	if shellCheckPat.MatchString(t) {
		return true
	}
	// 3) 实体 + 版本
	if entityVersionPat.MatchString(t) {
		return true
	}
	// 4) 核实动词 + 环境实体
	if verifyVerbPat.MatchString(t) && envEntityPat.MatchString(t) {
		return true
	}
	return false
}

// VerificationGateConfig 配置验证门禁。
type VerificationGateConfig struct {
	// Enabled 是否启用。默认 true。
	Enabled bool
}

// NewVerificationGateConfig 返回默认配置。
func NewVerificationGateConfig() VerificationGateConfig {
	return VerificationGateConfig{Enabled: true}
}

// IsZero 判断配置是否为空。
func (c VerificationGateConfig) IsZero() bool {
	return !c.Enabled
}

// VerificationGateMiddleware 在 LLMStage 之前对用户输入做确定性分类，
// 命中环境类问题时在 Envelope 上标记 `verify.required`，供 LLMStage 落地
// 为 tool_choice=required 的强制门禁。
func VerificationGateMiddleware(cfg VerificationGateConfig) Middleware {
	if cfg.IsZero() {
		return func(next core.Stage) core.Stage { return next }
	}
	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name() + ".verify-gate",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				if requiresVerification(env.Message.Text) {
					env.Set("verify.required", true)
				}
				return next.Process(ctx, env)
			},
		}
	}
}
