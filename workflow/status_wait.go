package workflow

// ============================================================================
// task_status 的服务端等待（wait mode）
//
// 背景（真实体验问题）：
//   工作流的分析 + 执行常持续数分钟到数十分钟。旧的 task_status 是「查一次即返回」，
//   于是 LLM 只能自己轮询——反复调用 task_status、每次之间还要靠 sleep 拖时间。
//   实测一次长任务能烧掉 20+ 次工具调用，全是「仍在分析中，让我继续等待」这类空转：
//     · 浪费大量 token 与调用轮次（可能撞上编排循环的步数上限）
//     · 对话记录被无意义的轮询卡片淹没
//     · LLM 自行决定的等待间隔既不稳定也不合理
//
// 解法：把轮询下沉到服务端。
//   agent 调一次 task_status(wait=true)，服务端在函数内阻塞轮询，直到工作流进入终态
//   （completed / failed / terminated）或超时才返回。agent 只在「成功/失败」时被唤醒。
//
// 三条必须守住的边界：
//  1. 必须有超时上限。否则一个卡死的工作流会把 agent 的这次工具调用永久挂住。
//  2. 必须响应 ctx 取消。用户点停止 / 客户端断开时要立刻退出，不能继续空转。
//  3. 超时返回的不是错误，而是「当前快照 + timedOut 标记」。让 agent 能自行决定
//     是继续等还是改用别的策略；报错会迫使它走异常处理分支。
// ============================================================================

import (
	"context"
	"time"
)

const (
	// statusWaitPollInterval 是服务端等待期间的状态查询间隔。
	//
	// 工作流状态变化是分钟级的，3s 足够及时；再密只是徒增 DB 读。
	statusWaitPollInterval = 3 * time.Second

	// statusWaitDefaultTimeout 是 wait 模式的默认超时。
	//
	// 取 10 分钟：既能覆盖多数「分析 + 前几个节点」的耗时，又不会让 agent 的一次
	// 工具调用长时间无响应。超时后 agent 拿到快照，可自行决定是否再等一轮。
	statusWaitDefaultTimeout = 10 * time.Minute

	// statusWaitMaxTimeout 是允许的最大超时上限。
	//
	// 即使调用方传了更大的值也会被截断——无上限等待等于把 agent 挂死。
	statusWaitMaxTimeout = 30 * time.Minute
)

// waitStatusResult 是 wait 模式的返回结构。
//
// 内嵌 StatusResult，保证与非 wait 模式的字段完全一致（agent 无需区分两种返回形态），
// 额外携带等待过程的元信息。
type waitStatusResult struct {
	*StatusResult

	// Waited 本次服务端等待的时长（人类可读，如 "3m12s"）。
	Waited string `json:"waited,omitempty"`

	// TimedOut 为 true 表示等待超时返回，工作流**仍在进行中**。
	// agent 可以再次调用继续等待，或转而检查节点详情。
	TimedOut bool `json:"timedOut,omitempty"`

	// Hint 给 agent 的下一步建议，避免它在超时后瞎猜。
	Hint string `json:"hint,omitempty"`
}

// waitForTerminal 阻塞轮询工作流状态，直到进入终态、超时或 ctx 被取消。
//
// timeout <= 0 时使用 statusWaitDefaultTimeout；超过 statusWaitMaxTimeout 会被截断。
// onProgress 非 nil 时会在每次轮询后回调，用于把等待进度推送给前端（避免界面看起来卡死）。
//
// 返回的 error 仅代表「查不到这个工作流」这类真实错误；超时不算错误，通过
// waitStatusResult.TimedOut 表达。
func waitForTerminal(
	ctx context.Context,
	mgr *Manager,
	wfID string,
	timeout time.Duration,
	onProgress func(*StatusResult, time.Duration),
) (*waitStatusResult, error) {
	if timeout <= 0 {
		timeout = statusWaitDefaultTimeout
	}
	if timeout > statusWaitMaxTimeout {
		timeout = statusWaitMaxTimeout
	}

	start := time.Now()

	// 先查一次：工作流可能已经结束，此时不该白等一个轮询间隔。
	st, err := mgr.GetStatus(wfID)
	if err != nil {
		return nil, err
	}
	if st.Status.IsTerminal() {
		return &waitStatusResult{StatusResult: st}, nil
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(statusWaitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 用户停止或连接断开：立即返回当前快照，不再等待。
			// 这里不返回 ctx.Err()，因为「已经拿到的状态」对调用方仍然有用。
			return &waitStatusResult{
				StatusResult: st,
				Waited:       time.Since(start).Truncate(time.Second).String(),
				TimedOut:     true,
				Hint:         "The wait was canceled (the user stopped it or the connection dropped). The task is still running in the background.",
			}, nil

		case <-deadline:
			return &waitStatusResult{
				StatusResult: st,
				Waited:       time.Since(start).Truncate(time.Second).String(),
				TimedOut:     true,
				Hint: "The wait timed out, but the task is still running. Call task_status(wait=true) again to keep waiting, " +
					"or use task_detail to inspect sub-task progress. NEVER switch to high-frequency polling.",
			}, nil

		case <-ticker.C:
			next, err := mgr.GetStatus(wfID)
			if err != nil {
				// 单次查询失败不终止等待（可能是瞬时 DB 争用），保留上一次快照继续。
				continue
			}
			st = next
			if st.Status.IsTerminal() {
				return &waitStatusResult{
					StatusResult: st,
					Waited:       time.Since(start).Truncate(time.Second).String(),
				}, nil
			}
			if onProgress != nil {
				onProgress(st, time.Since(start))
			}
		}
	}
}
