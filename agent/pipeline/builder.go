package pipeline

import (
	"sort"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// PipelineMode — 声明式阶段装配的「模式」词汇
// ============================================================================
//
// 对应 deepseek-harness 的 agent preset / "agent.cordis.yml" 插件花名册思想：
// 一个 bot 的 Pipeline 由哪一组 Stage 构成，应由一个具名「模式」驱动，
// 而不是散落在调用方各处的条件 append。
//
// 目前 Builder 已支持按模式携带（WithMode/Mode），配置侧也已接线：
// 键 pipeline.mode / bot.<id>.pipeline_mode + config.Builder.GetPipelineMode
// 决定当前模式（C2-b 已完成）。模式通过 ModeGroups 真正门控 stage/tool 花名册
// （engagement / heartbeat / lurk / code 各组由 PipelineMode 决定启用与否，
// 而非仅元数据携带）。standard 行为等价旧实现。
type PipelineMode string

const (
	// ModeStandard 标准模式：完整链路（enrich → recall → rhythm → llm，可选 engagement/heartbeat）。
	ModeStandard PipelineMode = "standard"
	// ModeLurkOnly 潜水模式：只学习不发言（对应 harness 的 lurk-only preset）。
	ModeLurkOnly PipelineMode = "lurk-only"
	// ModeCode 代码编排模式：启用 run_code 类工具，把多轮工具下推到运行时（对应 harness 的 code preset，C3）。
	ModeCode PipelineMode = "code"
)

// ============================================================================
// StageGroup — 可被模式整体启停的「阶段组」
// ============================================================================
//
// 把零散的 stage / tool 收敛成语义组，由 PipelineMode 决定哪些组启用。
// 调用方（botservice 装配、bot.New 工具注册）只问「本模式要不要这组」，
// 不再散落一堆 if mode == "..." 的条件 append，模式语义集中在 ModeGroups。
type StageGroup string

const (
	// GroupEngagement 参与决策组：是否进场/发言（engagement stage，Order=40）。
	GroupEngagement StageGroup = "engagement"
	// GroupHeartbeat 心跳组：自主唤醒与其频控重置（heartbeat-activity stage，Order=5）。
	GroupHeartbeat StageGroup = "heartbeat"
	// GroupLurk 潜水资源组：speak OFF 时仍富化并学习的阶段（lurk enricher / recall / rhythm / LLM 潜水分支）。
	GroupLurk StageGroup = "lurk"
	// GroupCode 代码工具组：bot 工作空间工具（sandbox_exec / read_file / run_code 等），bot.New 注册时按模式门控。
	GroupCode StageGroup = "code"
)

// ModeGroups 返回某模式下应启用的阶段/工具组集合。
//
// 语义（对应 harness 的 agent preset）：
//   - standard / code：完整链路，四组皆开（code 模式下 run_code 等始终可用，与标准一致）。
//   - lurk-only：只学不说——关掉 engagement（不决策发言）与 heartbeat（不自主发帖），
//     也不注册任何工作空间/代码工具（不执行动作）；仅保留潜水资源组。
//
// 未知模式一律回退到 standard（fail-open），保证不丢 stage、不丢工具。
func ModeGroups(m PipelineMode) map[StageGroup]bool {
	switch m {
	case ModeLurkOnly:
		return map[StageGroup]bool{GroupLurk: true}
	default: // ModeStandard / ModeCode / 未知
		return map[StageGroup]bool{
			GroupEngagement: true,
			GroupHeartbeat:  true,
			GroupLurk:       true,
			GroupCode:       true,
		}
	}
}

// ============================================================================
// Builder — 声明式 Stage 装配器
// ============================================================================
//
// 取代此前调用方手写 []core.StageInfo 字面量 + 条件 append/prepend 的易漂移写法：
// 新增 Stage 只需一行 Add/AddIf，Order 即其在链路中的相对位置；
// Build() 按 Order 升序产出，pipeline.New 亦会再次排序，最终顺序由 Order 唯一决定，
// 与 Add 调用次序无关。
type Builder struct {
	specs []core.StageInfo
	mode  PipelineMode
}

// NewBuilder 创建空装配器。
func NewBuilder() *Builder {
	return &Builder{}
}

// WithMode 携带装配模式（供 Mode() 读取；真正的门控由 ModeGroups 依据该模式完成）。
func (b *Builder) WithMode(m PipelineMode) *Builder {
	b.mode = m
	return b
}

// Mode 返回当前携带的装配模式。
func (b *Builder) Mode() PipelineMode {
	return b.mode
}

// Add 无条件注册一个 Stage。
func (b *Builder) Add(order int, stage core.Stage) *Builder {
	b.specs = append(b.specs, core.StageInfo{
		Stage:   stage,
		Order:   order,
		Enabled: true,
	})
	return b
}

// AddIf 仅在 cond 为真时注册（对应此前「按配置决定是否启用」的 Stage）。
// cond 为假时直接跳过，绝不把 nil Stage 写入装配结果。
func (b *Builder) AddIf(cond bool, order int, stage core.Stage) *Builder {
	if !cond {
		return b
	}
	return b.Add(order, stage)
}

// Build 返回按 Order 升序、保留 Enabled 的 StageInfo 切片。
// 此处预先排序仅为可读性与确定性；pipeline.New 会再次排序。
func (b *Builder) Build() []core.StageInfo {
	out := make([]core.StageInfo, len(b.specs))
	copy(out, b.specs)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Order < out[j].Order
	})
	return out
}
