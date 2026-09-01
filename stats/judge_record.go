package stats

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// JudgeRecorder — LLM 快判结果的异步落库
//
// 复用 Recorder 的模式（channel 缓冲 + 后台 goroutine 定时批量写入）：
// 判定发生在消息处理的关键路径上，落库不能阻塞它，更不能因 DB 抖动
// 把参与决策拖垮。channel 满时丢弃并告警，与 Recorder 的取舍一致。
// ============================================================================

// JudgeRecorder 收集 LLM 快判结果并异步写入 judge_records 表。
type JudgeRecorder struct {
	db     *gorm.DB
	logger *zap.SugaredLogger

	ch     chan dao.JudgeRecord
	stopCh chan struct{}
	wg     sync.WaitGroup

	flushInterval time.Duration
	batchSize     int
}

// NewJudgeRecorder 创建判定记录器。
// db 可为 nil（纯内存/测试模式），此时记录被静默丢弃。
func NewJudgeRecorder(db *gorm.DB, logger *zap.SugaredLogger) *JudgeRecorder {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &JudgeRecorder{
		db:            db,
		logger:        logger.With("stage", "stats_judge_recorder"),
		ch:            make(chan dao.JudgeRecord, 1024),
		stopCh:        make(chan struct{}),
		flushInterval: 5 * time.Second,
		batchSize:     100,
	}
}

// Start 启动后台写入 goroutine。
func (r *JudgeRecorder) Start() {
	r.wg.Add(1)
	go r.run()
}

// Stop 停止后台写入 goroutine，刷新剩余记录。
func (r *JudgeRecorder) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// Record 异步记录一次判定。非阻塞：channel 满时丢弃并告警。
//
// 调用方（参与决策链路）不应依赖本方法的返回值——落库失败绝不能影响
// 「是否参与」这个主决策。
func (r *JudgeRecorder) Record(ctx context.Context, rec dao.JudgeRecord) {
	if r.db == nil {
		return
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	select {
	case r.ch <- rec:
	default:
		r.logger.Warnw("judge recorder: channel full, record dropped",
			"bot_id", rec.BotID, "model", rec.Model)
	}
}

// SyncFlush 同步刷新缓冲的记录。用于测试。
func (r *JudgeRecorder) SyncFlush() {
	var batch []dao.JudgeRecord
	for {
		select {
		case rec := <-r.ch:
			batch = append(batch, rec)
		default:
			if len(batch) > 0 {
				if err := r.flushBatch(batch); err != nil {
					r.logger.Errorw("judge recorder: sync flush failed", "err", err)
				}
			}
			return
		}
	}
}

// run 是后台写入循环。
func (r *JudgeRecorder) run() {
	defer r.wg.Done()

	batch := make([]dao.JudgeRecord, 0, r.batchSize)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.flushBatch(batch); err != nil {
			r.logger.Errorw("judge recorder: flush failed",
				"count", len(batch), "err", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case rec := <-r.ch:
			batch = append(batch, rec)
			if len(batch) >= r.batchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-r.stopCh:
			// drain 剩余
			for {
				select {
				case rec := <-r.ch:
					batch = append(batch, rec)
				default:
					flush()
					return
				}
			}
		}
	}
}

// flushBatch 批量插入判定记录。
//
// 判定是逐条明细（不做聚合），故用 CreateInBatches 而非 UPSERT——
// 与 UsageDaily 的聚合路径刻意不同，见 dao.JudgeRecord 的说明。
func (r *JudgeRecorder) flushBatch(batch []dao.JudgeRecord) error {
	if len(batch) == 0 {
		return nil
	}
	return r.db.CreateInBatches(batch, r.batchSize).Error
}
