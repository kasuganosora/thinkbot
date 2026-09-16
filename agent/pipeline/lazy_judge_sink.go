package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ============================================================================
// LazyJudgeSink — 二级裁决判定结果的落库槽（标注语料）
//
// 设计哲学与 engagement.JudgeRecordSink 同源：判定结果此前只用于派生
// "是否 loop-back" 的决策，用完即弃——改了 prompt / 换了模型，无从判断变好
// 还是变坏。落库后攒几个月就是一份带标注的语料：哪些一级词表词误报率高、
// 哪类回复容易被误伤，一目了然，到时候词表怎么调、规则要不要退役全靠数据说话。
//
// 实现必须满足：非阻塞、失败不影响主决策（落库是旁路观测，不能拖垮派发）。
// 默认实现写 JSONL 到本地文件，errors 一律吞掉。
// ============================================================================

// LazyJudgeRecord 一次两级裁决的结果快照。
type LazyJudgeRecord struct {
	TS             time.Time `json:"ts"`              // 判定时间
	BotID          string    `json:"bot_id"`          // 所属 bot
	Channel        string    `json:"channel"`         // 会话空间
	Stage1Matched  bool      `json:"stage1_matched"`  // 一级正则是否召回
	ReplyLen       int       `json:"reply_len"`       // 回复长度（input feature）
	HadToolCalls   bool      `json:"had_tool_calls"`  // 本轮实际是否调了工具
	Lazy           bool      `json:"lazy"`            // 二级裁决：是否偷懒
	EntityAsserted bool      `json:"entity_asserted"` // 二级裁决：是否断言了环境实体
	Confidence     float64   `json:"confidence"`      // 二级裁决置信度 0..1
	Reason         string    `json:"reason"`          // 二级裁决理由
	FinalAction    string    `json:"final_action"`    // veto / no_judge / loopback / loopback_failed / suppressed
}

// LazyJudgeSink 判定结果的落库目标。接口定义在消费方（本包），实现可选注入。
type LazyJudgeSink interface {
	RecordLazyJudge(ctx context.Context, rec LazyJudgeRecord)
}

// fileLazyJudgeSink 把判定结果追加到本地 JSONL 文件（默认 data/lazy_judgments.jsonl）。
// 同步追加但极快（单行），errors 吞掉——绝不阻塞或中断主派发链路。
type fileLazyJudgeSink struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

// NewFileLazyJudgeSink 创建基于本地 JSONL 文件的落库槽。
// path 一般用 "data/lazy_judgments.jsonl"（相对于服务运行目录，即项目根）。
func NewFileLazyJudgeSink(path string) LazyJudgeSink {
	return &fileLazyJudgeSink{path: path}
}

func (s *fileLazyJudgeSink) RecordLazyJudge(_ context.Context, rec LazyJudgeRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		return // 序列化失败直接丢弃，不影响主链路
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return // 打开失败（权限/路径）直接丢弃
		}
		s.f = f
	}
	// 单行追加，best-effort；磁盘满等错误忽略。
	_, _ = s.f.Write(line)
}
