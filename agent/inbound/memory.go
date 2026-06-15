package inbound

import (
	"context"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// MemorySource — 内存消息源（用于测试和开发）
// ============================================================================

// MemorySource 是一个基于内存的消息源。
// 消息通过 Send() 方法注入，适用于单元测试和开发调试。
type MemorySource struct {
	name    string
	inCh    chan core.Message
	outCh   chan<- *core.Envelope
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewMemorySource 创建内存消息源。
// bufSize 控制内部缓冲区大小，0 为无缓冲。
func NewMemorySource(name string, bufSize int) *MemorySource {
	if bufSize < 0 {
		bufSize = 0
	}
	return &MemorySource{
		name: name,
		inCh: make(chan core.Message, bufSize),
		done: make(chan struct{}),
	}
}

// Name 返回 "memory" 或自定义名称。
func (m *MemorySource) Name() string {
	if m.name == "" {
		return "memory"
	}
	return m.name
}

// Start 启动内存源，将收到的消息转发到 Pipeline 输入通道。
func (m *MemorySource) Start(ctx context.Context, ch chan<- *core.Envelope) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	m.outCh = ch
	ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

	go m.loop(ctx)
	return nil
}

// Stop 优雅关闭内存源。
func (m *MemorySource) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	// 等待 loop 退出或超时
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send 向内存源注入一条消息。
// 如果源未启动或缓冲已满，Send 会阻塞。
func (m *MemorySource) Send(msg core.Message) {
	// 填充默认值
	if msg.Source == "" {
		msg.Source = m.Name()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	m.inCh <- msg
}

// TrySend 尝试发送消息，如果缓冲已满则返回 false。
func (m *MemorySource) TrySend(msg core.Message) bool {
	if msg.Source == "" {
		msg.Source = m.Name()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	select {
	case m.inCh <- msg:
		return true
	default:
		return false
	}
}

// loop 内部消息转发循环。
func (m *MemorySource) loop(ctx context.Context) {
	defer close(m.done)

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-m.inCh:
			if !ok {
				return
			}
			env := core.NewEnvelope(msg)
			select {
			case m.outCh <- env:
			case <-ctx.Done():
				return
			}
		}
	}
}
