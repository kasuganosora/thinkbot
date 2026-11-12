package bot

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

// LLMSnapshot 是一次成功解析后的不可变模型绑定。
// 正在进行的回合通过 context 钉住它；下一回合读取槽里的新指针。
type LLMSnapshot struct {
	Generation  uint64
	Main        llm.Provider
	Light       llm.Provider
	Vision      llm.Provider
	MainModel   *llm.Model
	LightModel  *llm.Model
	VisionModel *llm.Model
	MainDef     config.ModelDef
	LightDef    config.ModelDef
	VisionDef   config.ModelDef
}

func modelWithTemperature(def config.ModelDef) *llm.Model {
	m := llm.ChatModel(def.Model)
	m.Temperature = def.Temperature
	return m
}

// SnapshotFromBundle 用刚建好的原始客户端做一份快照。调用方随后可以换上 follow provider。
func SnapshotFromBundle(b *LLMBundle) *LLMSnapshot {
	if b == nil {
		return nil
	}
	snap := &LLMSnapshot{
		Main:      b.Main,
		MainDef:   b.MainDef,
		MainModel: modelWithTemperature(b.MainDef),
	}
	if b.Light != nil {
		snap.Light = b.Light
		snap.LightDef = b.LightDef
		snap.LightModel = modelWithTemperature(b.LightDef)
	}
	if b.Vision != nil {
		snap.Vision = b.Vision
		snap.VisionDef = b.VisionDef
		snap.VisionModel = modelWithTemperature(b.VisionDef)
	}
	return snap
}

// UsesProvider 报告这份快照是否绑定了指定服务商。
func (s *LLMSnapshot) UsesProvider(id string) bool {
	if s == nil || id == "" {
		return false
	}
	return s.MainDef.ProviderID == id || s.LightDef.ProviderID == id || s.VisionDef.ProviderID == id
}

// FollowBundle 把 bundle 上的客户端换成跟随槽的包装。快照里仍是原始客户端。
// 没有独立 light/vision 时保持 nil，避免把「未配置」变成「已配置」。
func FollowBundle(slot *LLMSlot, b *LLMBundle) *LLMBundle {
	if b == nil {
		return nil
	}
	out := *b
	out.Main = newFollow(slot, "main")
	if b.Light != nil {
		out.Light = newFollow(slot, "light")
	}
	if b.Vision != nil {
		out.Vision = newFollow(slot, "vision")
	}
	return &out
}

type ctxSnapKey struct{}

// WithLLMSnapshot 把本回合使用的快照钉进 context。
func WithLLMSnapshot(ctx context.Context, snap *LLMSnapshot) context.Context {
	if snap == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxSnapKey{}, snap)
}

// LLMSnapshotFrom 返回钉住的快照。没有时为 nil。
func LLMSnapshotFrom(ctx context.Context) *LLMSnapshot {
	if ctx == nil {
		return nil
	}
	snap, _ := ctx.Value(ctxSnapKey{}).(*LLMSnapshot)
	return snap
}

// LLMSlot 原子替换当前快照。Publish 不会打断已经读走旧指针的调用。
type LLMSlot struct {
	mu       sync.Mutex
	gen      uint64
	cur      atomic.Pointer[LLMSnapshot]
	decorate func(*LLMSnapshot) *LLMSnapshot
}

// SetDecorate 安装把原始客户端包上统计/配额的函数。热切换发布前会再包一次。
func (s *LLMSlot) SetDecorate(fn func(*LLMSnapshot) *LLMSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.decorate = fn
	s.mu.Unlock()
}

// NewLLMSlot 以初始快照创建槽。snap 可以为 nil。
func NewLLMSlot(snap *LLMSnapshot) *LLMSlot {
	s := &LLMSlot{}
	if snap != nil {
		s.gen = 1
		snap.Generation = 1
		s.cur.Store(snap)
	}
	return s
}

// Current 返回当前快照。可能为 nil。
func (s *LLMSlot) Current() *LLMSnapshot {
	if s == nil {
		return nil
	}
	return s.cur.Load()
}

// Publish 换上新快照并返回被换下的那份。并发 Publish 串行，不会拼出半份快照。
func (s *LLMSlot) Publish(next *LLMSnapshot) *LLMSnapshot {
	if s == nil || next == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	next.Generation = s.gen
	return s.cur.Swap(next)
}

// PublishRaw 先按 SetDecorate 包装原始客户端，再原子换上。
// 包装放在快照里面、跟随层外面，统计看到的是这次调用真正使用的模型和参数。
func (s *LLMSlot) PublishRaw(raw *LLMSnapshot) *LLMSnapshot {
	if s == nil || raw == nil {
		return nil
	}
	s.mu.Lock()
	decorate := s.decorate
	s.mu.Unlock()
	if decorate != nil {
		raw = decorate(raw)
	}
	return s.Publish(raw)
}

// followProvider 每次调用时解析快照：优先用 context 里钉住的，否则用槽的当前值。
type followProvider struct {
	slot *LLMSlot
	role string
}

func newFollow(slot *LLMSlot, role string) llm.Provider {
	return &followProvider{slot: slot, role: role}
}

// PinWork 把当前快照钉进这一次多步工作的 context。
// 已经钉住的保持不变。下一次独立工作再读最新快照，不是整段后台事务。
func (p *followProvider) PinWork(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if LLMSnapshotFrom(ctx) != nil || p == nil || p.slot == nil {
		return ctx
	}
	snap := p.slot.Current()
	if snap == nil {
		return ctx
	}
	return WithLLMSnapshot(ctx, snap)
}

func (p *followProvider) resolve(ctx context.Context) (llm.Provider, *llm.Model, config.ModelDef) {
	snap := LLMSnapshotFrom(ctx)
	if snap == nil && p.slot != nil {
		// 没有钉住快照的后台调用每次读当前槽。这不是整段工作流的事务：
		// 下一次调用如果槽已经换过，就用新的快照，不会和上一次调用混在同一次请求里。
		snap = p.slot.Current()
	}
	if snap == nil {
		return nil, nil, config.ModelDef{}
	}
	switch p.role {
	case "light":
		if snap.Light != nil {
			return snap.Light, snap.LightModel, snap.LightDef
		}
		return snap.Main, snap.MainModel, snap.MainDef
	case "vision":
		if snap.Vision != nil {
			return snap.Vision, snap.VisionModel, snap.VisionDef
		}
		return nil, nil, config.ModelDef{}
	default:
		return snap.Main, snap.MainModel, snap.MainDef
	}
}

func bindSnapshotParams(ctx context.Context, params llm.GenerateParams, model *llm.Model, def config.ModelDef) llm.GenerateParams {
	if model != nil {
		params.Model = model
	}
	keep := llm.KeptParamsFrom(ctx)
	if !keep.Temperature && def.Temperature != nil {
		params.Temperature = def.Temperature
	}
	if !keep.TopP && def.TopP != nil {
		params.TopP = def.TopP
	}
	if !keep.MaxTokens && def.MaxTokens > 0 {
		mt := def.MaxTokens
		params.MaxTokens = &mt
	}
	if !keep.ReasoningEffort && params.ReasoningEffort != nil && model != nil {
		effort := llm.NormalizeReasoningEffort(model.ID, *params.ReasoningEffort)
		if effort == "" {
			params.ReasoningEffort = nil
		} else {
			params.ReasoningEffort = &effort
		}
	}
	return params
}

func (p *followProvider) Name() string {
	inner, _, _ := p.resolve(context.Background())
	if inner == nil {
		return "llm"
	}
	return inner.Name()
}

func (p *followProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	inner, model, def := p.resolve(ctx)
	if inner == nil {
		return nil, errNoSnapshot
	}
	params = bindSnapshotParams(ctx, params, model, def)
	return inner.DoGenerate(ctx, params)
}

func (p *followProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	inner, model, def := p.resolve(ctx)
	if inner == nil {
		return nil, errNoSnapshot
	}
	params = bindSnapshotParams(ctx, params, model, def)
	return inner.DoStream(ctx, params)
}

var errNoSnapshot = errString("llm snapshot is not available")

type errString string

func (e errString) Error() string { return string(e) }
