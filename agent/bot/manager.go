package bot

import (
	"context"
	"fmt"
	"sync"

	"github.com/kasuganosora/thinkbot/util/errs"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// ============================================================================
// BotManager — 多 Bot 生命周期管理器
// ============================================================================

// BotManager 管理平台中所有 Bot 的注册、查找和生命周期。
//
// 一个应用有一个 BotManager，它负责：
//   - 注册/注销 Bot
//   - 按 ID 查找 Bot
//   - 启动/停止所有 Bot（RunAll / StopAll）
//   - 提供运行状态查询
//
// BotManager 是线程安全的，支持运行时动态注册/注销 Bot。
type BotManager struct {
	mu     sync.RWMutex
	bots   map[string]*Bot
	logger *zap.SugaredLogger
	tracer trace.Tracer
	errCh  chan error // 后续错误通道（RunAll 就绪后的崩溃错误）
}

// NewBotManager 创建 Bot 管理器。
func NewBotManager(logger *zap.SugaredLogger, tp trace.TracerProvider) *BotManager {
	return &BotManager{
		bots:   make(map[string]*Bot),
		logger: logger.With("component", "bot_manager"),
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/bot/manager"),
	}
}

// Register 注册一个 Bot。
// 如果 ID 已存在，返回错误。
func (m *BotManager) Register(bot *Bot) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.bots[bot.ID]; exists {
		return fmt.Errorf("bot_manager: bot %q already registered", bot.ID)
	}

	m.bots[bot.ID] = bot
	m.logger.Infow("bot registered",
		"bot_id", bot.ID,
		"bot_name", bot.Name,
		"channels", len(bot.channels))

	return nil
}

// Unregister 注销一个 Bot。
// 如果 Bot 正在运行，先 Stop 再注销。
// 如果 ID 不存在，返回 false。
func (m *BotManager) Unregister(id string) bool {
	m.mu.Lock()
	bot, exists := m.bots[id]
	if !exists {
		m.mu.Unlock()
		return false
	}
	delete(m.bots, id)
	m.mu.Unlock()

	// 在锁外执行 Stop/Close，避免阻塞其他 Manager 操作
	bot.Stop()
	bot.Close()
	m.logger.Infow("bot unregistered", "bot_id", id)

	return true
}

// Get 按 ID 查找 Bot。
func (m *BotManager) Get(id string) (*Bot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bot, ok := m.bots[id]
	return bot, ok
}

// List 返回所有已注册 Bot 的快照。
func (m *BotManager) List() []*Bot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bots := make([]*Bot, 0, len(m.bots))
	for _, b := range m.bots {
		bots = append(bots, b)
	}
	return bots
}

// Count 返回已注册 Bot 数量。
func (m *BotManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bots)
}

// ErrCh 返回一个只读错误通道，用于持续监听 Bot 启动后的崩溃错误。
// 调用方应在 RunAll 返回后开始监听此通道。
// 如果尚未调用 RunAll，返回 nil。
func (m *BotManager) ErrCh() <-chan error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.errCh
}

// RunAll 启动所有已注册的 Bot。
// 每个 Bot 在独立 goroutine 中运行。
// RunAll 等待所有 Bot 完成初始化（Channel 启动、worker 就绪）后返回。
// 如果任何 Bot 的 Run 方法在初始化阶段返回错误，RunAll 返回该错误。
func (m *BotManager) RunAll(ctx context.Context) error {
	m.mu.RLock()
	bots := make([]*Bot, 0, len(m.bots))
	for _, b := range m.bots {
		bots = append(bots, b)
	}
	m.mu.RUnlock()

	m.logger.Infow("starting all bots", "count", len(bots))

	errCh := make(chan error, len(bots))

	for _, b := range bots {
		go func(bot *Bot) {
			if err := bot.Run(ctx); err != nil {
				errCh <- errs.Wrapf(err, "bot %q", bot.ID)
			}
		}(b)
	}

	// 等待所有 Bot 就绪或出错
	for _, b := range bots {
		select {
		case <-b.Ready():
			m.logger.Debugw("bot ready", "bot_id", b.ID)
		case err := <-errCh:
			// 某个 Bot 在初始化阶段失败
			return err
		case <-ctx.Done():
			return errs.Wrap(ctx.Err(), "bot_manager: context cancelled while waiting for bots")
		}
	}

	// 非阻塞检查是否有在就绪后立即失败的
	select {
	case err := <-errCh:
		return err
	default:
	}

	// 所有 Bot 就绪后，启动后台 goroutine 监听后续错误并记录日志
	// （调用方可通过 ErrCh() 方法获取持续错误流）
	m.mu.Lock()
	if m.errCh == nil {
		m.errCh = make(chan error, len(bots))
	}
	errChForMonitor := m.errCh
	m.mu.Unlock()

	go func() {
		for {
			select {
			case err := <-errCh:
				if err != nil {
					m.logger.Errorw("bot crashed after startup",
						"err", err)
					select {
					case errChForMonitor <- err:
					default:
						// errCh 满了，丢弃（调用方未消费）
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	m.logger.Infow("all bots started and ready", "count", len(bots))
	return nil
}

// StopAll 停止所有已注册的 Bot 并释放资源。
func (m *BotManager) StopAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.logger.Infow("stopping all bots", "count", len(m.bots))

	for _, b := range m.bots {
		b.Stop()
		b.Close()
	}

	m.logger.Infow("all bots stop signal sent")
}

// BotInfo 是 Bot 的摘要信息，用于状态查询。
type BotInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
	Workers  int      `json:"workers"`
}

// Info 返回所有 Bot 的摘要信息。
func (m *BotManager) Info() []BotInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]BotInfo, 0, len(m.bots))
	for _, b := range m.bots {
		chNames := make([]string, 0, len(b.channels))
		for _, ch := range b.channels {
			chNames = append(chNames, ch.Name())
		}
		infos = append(infos, BotInfo{
			ID:       b.ID,
			Name:     b.Name,
			Channels: chNames,
			Workers:  b.Config.Workers,
		})
	}
	return infos
}
