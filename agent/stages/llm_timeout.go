package stages

import (
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// partialSteps 累计编排中已完成的步骤。编排被墙钟硬上限打断时返回 error 而没有结果，
// 这些步骤的 token 用量与工具调用只能从这里拿到。
type partialSteps struct {
	mu    sync.Mutex
	steps []llm.StepResult
}

// wrap 在保留原 OnStep 行为的前提下记录每个完成的步骤。
func (p *partialSteps) wrap(next func(*llm.StepResult) *llm.GenerateParams) func(*llm.StepResult) *llm.GenerateParams {
	return func(sr *llm.StepResult) *llm.GenerateParams {
		if sr != nil {
			p.mu.Lock()
			p.steps = append(p.steps, llm.StepResult{
				Usage:     sr.Usage,
				ToolCalls: sr.ToolCalls,
			})
			p.mu.Unlock()
		}
		if next != nil {
			return next(sr)
		}
		return nil
	}
}

func (p *partialSteps) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.steps)
}

// result 返回已完成步骤合成的结果（Usage 为各步之和），供记账与超时回复。
func (p *partialSteps) result() *llm.GenerateResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := &llm.GenerateResult{FinishReason: llm.FinishReasonStop}
	res.Steps = append([]llm.StepResult(nil), p.steps...)
	for i := range res.Steps {
		res.Usage.Add(&res.Steps[i].Usage)
	}
	return res
}

// calledTool 判断已完成步骤里是否调用过某个工具。
func (p *partialSteps) calledTool(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.steps {
		for _, tc := range st.ToolCalls {
			if tc.ToolName == name {
				return true
			}
		}
	}
	return false
}

// hardTimeoutResult 构造墙钟硬超时回合的结果：Usage 为已完成步骤之和（照常记账）。
// notify=true 时 Text 是给用户的一句超时说明，调用方沿正常出站路径发出（私聊，或群里
// 直接 @/回复了 bot）；notify=false（心跳 / 潜水 / 群聊旁听）时只记账、不出站。
func (s *LLMStage) hardTimeoutResult(env *core.Envelope, partial *partialSteps, limit time.Duration, lurkMode, heartbeatMode bool) (*llm.GenerateResult, bool) {
	res := partial.result()
	if lurkMode || heartbeatMode || env.IsOutreach() {
		return res, false
	}
	if !isPrivateChat(env) && !env.Message.Mentioned {
		return res, false
	}
	text := fmt.Sprintf("⚠️ 这一轮处理超过了 %s 的时限，被系统中止了，没能给出完整回复。", humanDuration(limit))
	if partial.calledTool("task") {
		text += "已提交的后台任务仍在继续运行，完成后我会在这里告诉你结果。"
	} else {
		text += "你可以回复「继续」让我接着做，或者把任务拆小一点再发给我。"
	}
	if s.config.RequireReplyControl {
		text += "\n" + replyControlDelimiter + `{"send": true}`
	}
	res.Text = text
	return res, true
}

// humanDuration 把时长写成「15 分钟」这类中文短语。
func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	}
	return d.Round(time.Second).String()
}
