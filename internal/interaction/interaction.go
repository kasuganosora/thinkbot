// Package interaction 提供「向用户提问—等待用户应答」的进程内交互原语，
// 是跨平台 user_choice 工具的核心枢纽。
//
// 设计意图：
//   - 工具侧（user_choice）注册一个 Question 并用 Wait 阻塞等待用户选择；
//   - 各平台 channel（web / telegram / misskey）收到用户输入后，用统一的
//     Answer 结构调用 Resolve 回填结果并唤醒等待者；
//   - channel 与工具之间只通过 questionID 解耦，不侵入彼此的消息主链路。
//
// 进程内单例（DefaultRegistry）即可满足单租户 bot 的需求：多 bot、多 channel
// 共享同一注册表，以 questionID 全局唯一为前提。
package interaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Status 是问题的生命周期状态。
//
// 状态机（终态不可逆，任何再次变更都返回 ErrAlreadyResolved）：
//
//	pending --Resolve-->   answered
//	pending --deadline-->  timeout
//	pending --Cancel---->  cancelled
type Status string

const (
	// StatusPending 已注册，等待应答。
	StatusPending Status = "pending"
	// StatusAnswered 已被 Resolve 回填。
	StatusAnswered Status = "answered"
	// StatusTimeout 等待超时。
	StatusTimeout Status = "timeout"
	// StatusCancelled 被主动取消（会话中止等）。
	StatusCancelled Status = "cancelled"
)

// Mode 选择模式。
type Mode string

const (
	// ModeSingle 单选：只能选一个选项。
	ModeSingle Mode = "single"
	// ModeMulti 多选：可选多个选项。
	ModeMulti Mode = "multi"
)

// Via 应答来源平台。
type Via string

const (
	ViaWeb      Via = "web"
	ViaTelegram Via = "telegram"
	ViaMisskey  Via = "misskey"
)

// IsValidMode 判断 Mode 是否合法。
func IsValidMode(m Mode) bool {
	return m == ModeSingle || m == ModeMulti
}

// IsValidVia 判断 Via 是否合法。
func IsValidVia(v Via) bool {
	switch v {
	case ViaWeb, ViaTelegram, ViaMisskey:
		return true
	}
	return false
}

// 包级错误哨兵：调用方一律用 errors.Is 判断，保证 API 语义稳定。
var (
	// ErrQuestionNotFound 问题不存在（未注册或已清理）。
	ErrQuestionNotFound = errors.New("interaction: 问题不存在")
	// ErrDuplicateID 问题 ID 已注册。
	ErrDuplicateID = errors.New("interaction: 问题 ID 已注册")
	// ErrInvalidQuestion 问题字段非法（ID/正文为空等）。
	ErrInvalidQuestion = errors.New("interaction: 问题字段非法")
	// ErrInvalidOptions 选项数量非法（须 1~8 个）。
	ErrInvalidOptions = errors.New("interaction: 选项数量非法")
	// ErrInvalidMode 模式非法。
	ErrInvalidMode = errors.New("interaction: 模式非法")
	// ErrInvalidVia 应答来源非法。
	ErrInvalidVia = errors.New("interaction: 应答来源非法")
	// ErrInvalidSelected 选中下标非法（越界或单选选了多个）。
	ErrInvalidSelected = errors.New("interaction: 选中下标非法")
	// ErrAlreadyResolved 问题已处于终态，不能再变更。
	ErrAlreadyResolved = errors.New("interaction: 问题已处于终态")
	// ErrTimeout 等待超时。
	ErrTimeout = errors.New("interaction: 等待超时")
	// ErrCancelled 问题已取消。
	ErrCancelled = errors.New("interaction: 问题已取消")
)

const (
	// DefaultTimeoutSecs 是 TimeoutSecs 未设置（0 或负值）时的缺省等待秒数。
	DefaultTimeoutSecs = 600
	// MinOptions / MaxOptions 选项数量的合法区间（1~8）。
	// 上限 8 是三平台的交集约束：Telegram inline keyboard 行列有限，
	// Misskey 纯文本编号列表太长可读性差。
	MinOptions = 1
	MaxOptions = 8
)

// Option 单个候选选项。
type Option struct {
	// ID 选项的稳定标识。注册时留空则由 RegisterQuestion 按序自动补为
	// "o0"、"o1"…… 。
	//
	// 为什么需要 ID：渲染层（web 卡片 / telegram 按钮 callback_data）回填的是
	// 「用户点了哪个选项」，用下标做线传标识很脆——一旦渲染侧与注册侧的选项
	// 顺序不一致就会静默选错项。统一用 ID 线传、由本包负责 ID→下标的翻译，
	// 内部仍以下标为唯一真值（Answer.Selected）。
	ID string `json:"id"`
	// Label 展示文案（必填）。
	Label string `json:"label"`
	// Description 补充说明，可为空。
	Description string `json:"description,omitempty"`
}

// Question 一次向用户提出的问题。
//
// 设计要点：按值传递并在注册时拷贝入注册表，调用方无法绕过锁改动内部状态；
// Status 由 Registry 统一维护（注册时强制置 pending），外部只读。
type Question struct {
	// ID 全局唯一问题 ID（三个平台 channel 用它回填应答）。
	ID string
	// BotID 归属 bot（路由与审计用）。
	BotID string
	// ChatID 归属会话空间（web: sessionID / telegram: chatID / misskey: channelID）。
	// 用于校验应答来自同一会话，防止 A 会话的选择 resolve B 会话的问题。
	ChatID string
	// Question 问题正文。
	Question string
	// Options 候选选项（1~8 个）。
	Options []Option
	// Mode single / multi。
	Mode Mode
	// InputHint 自由输入框的引导文案，可为空。
	InputHint string
	// TimeoutSecs 等待超时秒数；注册时为 0（或负）则置 DefaultTimeoutSecs。
	TimeoutSecs int
	// Status 状态机快照（由 Registry 维护，注册时读回）。
	Status Status
}

// validate 校验问题字段（不含 ID 唯一性，那由 Registry 检查）。
func (q *Question) validate() error {
	if q.ID == "" || q.Question == "" {
		return fmt.Errorf("%w: ID 与问题正文均不能为空", ErrInvalidQuestion)
	}
	if len(q.Options) < MinOptions || len(q.Options) > MaxOptions {
		return fmt.Errorf("%w: 须为 %d~%d 个，实际 %d 个",
			ErrInvalidOptions, MinOptions, MaxOptions, len(q.Options))
	}
	for i, opt := range q.Options {
		if opt.Label == "" {
			return fmt.Errorf("%w: 第 %d 个选项缺少 label", ErrInvalidQuestion, i)
		}
	}
	if !IsValidMode(q.Mode) {
		return fmt.Errorf("%w: %q", ErrInvalidMode, q.Mode)
	}
	return nil
}

// Answer 用户应答的统一结构，各平台解析后统一回填。
type Answer struct {
	// Selected 选中的选项下标列表（0 起）。可为空（用户只填了自定义输入）。
	Selected []int
	// CustomInput 用户自由输入的文本，可为空。
	CustomInput string
	// Via 应答来源平台。
	Via Via
}

// validate 校验应答与问题匹配（下标越界、单选多选）。
func (a *Answer) validate(q *Question) error {
	if !IsValidVia(a.Via) {
		return fmt.Errorf("%w: %q", ErrInvalidVia, a.Via)
	}
	if len(a.Selected) == 0 && a.CustomInput == "" {
		return fmt.Errorf("%w: 未选择任何选项且无自定义输入", ErrInvalidSelected)
	}
	seen := make(map[int]bool, len(a.Selected))
	for _, idx := range a.Selected {
		if idx < 0 || idx >= len(q.Options) {
			return fmt.Errorf("%w: 下标 %d 越界（共 %d 个选项）",
				ErrInvalidSelected, idx, len(q.Options))
		}
		if seen[idx] {
			return fmt.Errorf("%w: 下标 %d 重复", ErrInvalidSelected, idx)
		}
		seen[idx] = true
	}
	if q.Mode == ModeSingle && len(a.Selected) > 1 {
		return fmt.Errorf("%w: 单选模式下选了 %d 个", ErrInvalidSelected, len(a.Selected))
	}
	return nil
}

// entry 是注册表中单个问题的完整状态。
type entry struct {
	question Question
	// done 应答/超时/取消时关闭，唤醒所有等待者（close 广播语义）。
	done chan struct{}
	// answer 终态为 answered 时的应答内容（在 done 关闭前写入）。
	answer Answer
}

// Registry 是问题等待注册表（并发安全）。
//
// 每个问题一个 entry：Resolve/超时/取消在锁内置终态并关闭 done channel，
// Wait 阻塞在 done 上，天然支持多个等待者同时被唤醒。
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// PollCreator 是平台原生的投票创建函数。
// 各 channel（如 Misskey）在启动时注册自己的实现，
// user_choice 工具对非 web 平台调用此函数创建原生投票帖
// （而非返回 unsupported 降级为纯文本）。
//
// 参数与 MisskeyChannel.CreatePollNote 签名一致：
//   ctx: 上下文
//   question: 题目文本
//   replyID: 回复目标帖子 ID（可选）
//   options: 选项文本列表
//   multiple: 是否多选
//   timeoutSecs: 过期时间（秒）
//   questionID: 关联的 interaction questionID
//
// 返回新建帖子 ID（noteID），用于日志和追踪。
type PollCreator func(ctx context.Context, question, replyID string, options []string, multiple bool, timeoutSecs int, questionID string) (string, error)

// pollCreators 按 platform 注册的投票创建器（如 "misskey" → MisskeyChannel.CreatePollNote）。
var (
	pollCreatorsMu sync.RWMutex
	pollCreators   map[string]PollCreator
)

func init() {
	pollCreators = make(map[string]PollCreator)
}

// RegisterPollCreator 为指定 platform 注册投票创建器。
// 覆盖已有注册（同 platform 后注册胜出）。
func RegisterPollCreator(platform string, fn PollCreator) {
	pollCreatorsMu.Lock()
	pollCreators[platform] = fn
	pollCreatorsMu.Unlock()
}

// GetPollCreator 查找指定 platform 的投票创建器。未注册返回 nil。
func GetPollCreator(platform string) PollCreator {
	pollCreatorsMu.RLock()
	defer pollCreatorsMu.RUnlock()
	return pollCreators[platform]
}

// NewRegistry 创建注册表。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
}

// defaultRegistry 是进程内共享注册表。
// 单租户单进程部署下足够；工具与 channel 都通过它交互。
var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
)

// Default 返回进程级单例注册表。
func Default() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewRegistry()
	})
	return defaultRegistry
}

// RegisterQuestion 注册问题并返回注册后的快照（含规范化后的 TimeoutSecs
// 与初始 Status）。ID 重复或字段非法返回错误。
func (r *Registry) RegisterQuestion(q Question) (Question, error) {
	if err := q.validate(); err != nil {
		return Question{}, err
	}
	if q.TimeoutSecs <= 0 {
		q.TimeoutSecs = DefaultTimeoutSecs
	}
	q.Status = StatusPending

	r.mu.Lock()
	if _, exists := r.entries[q.ID]; exists {
		r.mu.Unlock()
		return Question{}, fmt.Errorf("%w: %s", ErrDuplicateID, q.ID)
	}
	// 拷贝 Options 切片，隔离调用方后续修改。
	opts := make([]Option, len(q.Options))
	copy(opts, q.Options)
	// 补齐缺失的选项 ID：渲染层与回填链路都以 ID 为线传标识，绝不能为空。
	for i := range opts {
		if opts[i].ID == "" {
			opts[i].ID = fmt.Sprintf("o%d", i)
		}
	}
	q.Options = opts
	r.entries[q.ID] = &entry{question: q, done: make(chan struct{})}
	r.mu.Unlock()
	return q, nil
}

// Lookup 返回问题的只读快照（不存在返回 ErrQuestionNotFound）。
func (r *Registry) Lookup(questionID string) (Question, error) {
	r.mu.Lock()
	e, ok := r.entries[questionID]
	r.mu.Unlock()
	if !ok {
		return Question{}, fmt.Errorf("%w: %s", ErrQuestionNotFound, questionID)
	}
	// 拷贝快照，防止外部通过共享切片改动内部状态。
	snap := e.question
	opts := make([]Option, len(snap.Options))
	copy(opts, snap.Options)
	snap.Options = opts
	return snap, nil
}

// Resolve 回填应答并唤醒所有等待者。
//
// 已处于终态（answered/timeout/cancelled）返回 ErrAlreadyResolved；
// 校验失败返回对应错误且不改状态。
func (r *Registry) Resolve(questionID string, ans Answer) error {
	r.mu.Lock()
	e, ok := r.entries[questionID]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrQuestionNotFound, questionID)
	}
	if e.question.Status != StatusPending {
		r.mu.Unlock()
		return fmt.Errorf("%w: 当前状态 %s", ErrAlreadyResolved, e.question.Status)
	}
	if err := ans.validate(&e.question); err != nil {
		r.mu.Unlock()
		return err
	}
	// 拷贝 Selected，隔离调用方切片。
	sel := make([]int, len(ans.Selected))
	copy(sel, ans.Selected)
	e.answer = Answer{Selected: sel, CustomInput: ans.CustomInput, Via: ans.Via}
	e.question.Status = StatusAnswered
	close(e.done)
	r.mu.Unlock()
	return nil
}

// ResolveFrom 是带「会话归属校验」的 Resolve：只有来自问题所属会话空间的
// 应答才被接受。
//
// 为什么必须有这道校验：questionID 会随事件流下发到渲染层，若回填接口只认
// questionID，任何登录态请求都能替别的会话作答（A 会话的人替 B 会话点选）。
// chatID 传空表示调用方无法提供会话上下文（如内部调用），此时退回 Resolve
// 的行为——**注意这是宽松分支，对外接口必须传值**。
func (r *Registry) ResolveFrom(questionID, chatID string, ans Answer) error {
	if chatID != "" {
		r.mu.Lock()
		e, ok := r.entries[questionID]
		if !ok {
			r.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrQuestionNotFound, questionID)
		}
		want := e.question.ChatID
		r.mu.Unlock()
		// 问题自身没记会话空间时不做校验（老数据/非会话场景），有则必须相等。
		if want != "" && want != chatID {
			return fmt.Errorf("%w: 应答来自会话 %q，问题属于会话 %q",
				ErrQuestionNotFound, chatID, want)
		}
	}
	return r.Resolve(questionID, ans)
}

// IndicesForOptionIDs 把渲染层回填的选项 ID 列表翻译成内部下标列表。
//
// 未知 ID 一律报 ErrInvalidSelected —— 静默丢弃会让用户「点了却没生效」，
// 比直接报错难查得多。
func (r *Registry) IndicesForOptionIDs(questionID string, ids []string) ([]int, error) {
	q, err := r.Lookup(questionID)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(q.Options))
	for i, opt := range q.Options {
		index[opt.ID] = i
	}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		idx, ok := index[id]
		if !ok {
			return nil, fmt.Errorf("%w: 未知选项 ID %q", ErrInvalidSelected, id)
		}
		out = append(out, idx)
	}
	return out, nil
}

// Cancel 主动取消问题（会话中止等场景），等待者收到 ErrCancelled。
// 已是终态时返回 ErrAlreadyResolved，不存在返回 ErrQuestionNotFound。
func (r *Registry) Cancel(questionID string) error {
	return r.finalize(questionID, StatusCancelled)
}

// Wait 阻塞等待问题到达终态，返回终态快照与应答（仅 answered 态有意义）。
//
// 超时语义：本方法用 ctx 控制等待，同时以问题自身的 TimeoutSecs 为上限——
// 到达问题超时上限时置 timeout 终态（谁先到算谁）；ctx 取消同样会取消问题
// （工具的执行 ctx 被中止意味着本轮编排已终止，继续等待没有意义）。
func (r *Registry) Wait(ctx context.Context, questionID string) (Question, Answer, error) {
	r.mu.Lock()
	e, ok := r.entries[questionID]
	if !ok {
		r.mu.Unlock()
		return Question{}, Answer{}, fmt.Errorf("%w: %s", ErrQuestionNotFound, questionID)
	}
	budget := time.Duration(e.question.TimeoutSecs) * time.Second
	r.mu.Unlock()

	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	select {
	case <-e.done:
		// 终态：answered / timeout / cancelled。
		r.mu.Lock()
		snap := e.question
		ans := e.answer
		status := snap.Status
		r.mu.Unlock()
		switch status {
		case StatusAnswered:
			return snap, ans, nil
		case StatusCancelled:
			return snap, Answer{}, ErrCancelled
		default: // StatusTimeout
			return snap, Answer{}, ErrTimeout
		}
	case <-ctx.Done():
		// 等待方 ctx 结束：编排被中止，或等待预算（问题自身 TimeoutSecs /
		// 外层 deadline）先于用户作答到达。此时必须把问题落到终态，否则
		// 注册表里会留下永远 pending 的悬挂条目。
		//
		// 终态语义按原因区分：deadline 到了是 timeout，主动取消是 cancelled
		// （此前一律走 Cancel 置 cancelled，与包头状态机文档不符，
		//  且让「超时」与「被中止」在监控上不可区分）。
		err := ctx.Err()
		status := StatusCancelled
		timedOut := errors.Is(err, context.DeadlineExceeded)
		if timedOut {
			status = StatusTimeout
		}
		// 竞态兜底：用户可能正好在 ctx 结束的同一瞬间完成 Resolve。
		// 那种情况下问题已是 answered，绝不能把它改写成超时并丢弃应答
		// ——用户明明点了，回合却告诉模型「超时」是最难查的一类错。
		if ferr := r.finalize(questionID, status); errors.Is(ferr, ErrAlreadyResolved) {
			r.mu.Lock()
			snap := e.question
			ans := e.answer
			r.mu.Unlock()
			if snap.Status == StatusAnswered {
				return snap, ans, nil
			}
		}
		if timedOut {
			return Question{ID: questionID}, Answer{}, ErrTimeout
		}
		return Question{ID: questionID}, Answer{}, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
}

// finalize 把 pending 问题置为指定终态并唤醒等待者。
// 已是终态返回 ErrAlreadyResolved，不存在返回 ErrQuestionNotFound。
func (r *Registry) finalize(questionID string, status Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[questionID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrQuestionNotFound, questionID)
	}
	if e.question.Status != StatusPending {
		return fmt.Errorf("%w: 当前状态 %s", ErrAlreadyResolved, e.question.Status)
	}
	e.question.Status = status
	close(e.done)
	return nil
}

// cleanup 移除已到终态的问题条目（当前由后续按需清理，保持注册表不膨胀；
// pending 问题绝不能被清理，否则等待者永远阻塞在无人的 entry 上）。
func (r *Registry) cleanup(questionID string) {
	r.mu.Lock()
	if e, ok := r.entries[questionID]; ok && e.question.Status != StatusPending {
		delete(r.entries, questionID)
	}
	r.mu.Unlock()
}

// CleanupFinal 移除指定问题（仅终态可移除）。供工具在 Wait 返回后调用，
// 防止长跑进程的注册表无限增长。
func (r *Registry) CleanupFinal(questionID string) {
	r.cleanup(questionID)
}

// PendingCount 返回当前 pending 的问题数（测试与监控用）。
func (r *Registry) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		if e.question.Status == StatusPending {
			n++
		}
	}
	return n
}
