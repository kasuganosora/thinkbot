package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// 向量重建进度阶段。页面按 phase 展示，不阻塞请求。
const (
	VecRebuildSelect = "select"
	VecRebuildDelete = "delete"
	VecRebuildWrite  = "write"
	VecRebuildDone   = "done"
	VecRebuildError  = "error"
)

// vecRebuildProgressEvery 是写入阶段上报间隔。最后一条总会再报一次。
const vecRebuildProgressEvery = 20

// VecRebuildProgress 是一次重建的进度快照。
// Total 是选中的记忆条数，Indexed 是已经尝试写入向量的条数。
type VecRebuildProgress struct {
	Phase   string
	Total   int
	Indexed int
}

func reportVecRebuild(fn func(VecRebuildProgress), p VecRebuildProgress) {
	if fn != nil {
		fn(p)
	}
}

// RebuildBotVectors 把该 bot 已持久化的分层记忆重新写入 sqlite-vec。
//
// 进程只有一份 SQLite（data/thinkbot.db），memory_vec 也是全进程一张表。
// 因此这里不会 DROP 整表，只按已确认归属的 tiered entry_id 删除再插入。
//
// 纳入规则：
//   - scope bot:<botID> 且 metadata.bot_id 不冲突
//   - 非 bot scope 且 metadata.bot_id 明确等于该 bot
//   - 无归属或冲突的旧条目不猜测、不删除，历史会话不能代替归属证据
//
// live 非空且当前没有可用索引时，会 TryEnableVec + OpenVecIndex 并挂回这个 store，
// 这样部署后不必再重启一次才能写索引。
// onProgress 可为 nil。删除结束后，以及写入每 20 条和最后一条时会回调。
func RebuildBotVectors(ctx context.Context, gdb *gorm.DB, botID string, live *TieredStore, onProgress func(VecRebuildProgress)) (int, error) {
	if botID == "" {
		return 0, fmt.Errorf("bot id is required")
	}
	if gdb == nil && live != nil {
		gdb = live.db
	}
	if gdb == nil {
		return 0, fmt.Errorf("sqlite-vec is not available")
	}
	fail := func(total int, err error) (int, error) {
		reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildError, Total: total})
		return 0, err
	}
	reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildSelect})
	idx, err := ensureVecIndex(gdb, live)
	if err != nil {
		return fail(0, err)
	}

	var rows []dao.TieredMemoryModel
	if err := gdb.WithContext(ctx).Find(&rows).Error; err != nil {
		return fail(0, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(0, err)
	}
	selected := selectVecRebuildRows(botID, rows)
	total := len(selected)
	reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildSelect, Total: total})
	// Both memory tables share the bot vector bucket. Rebuilding tiered rows
	// must not erase memory_entries vectors from that same bucket.
	ids := make([]string, 0, len(selected))
	for _, row := range selected {
		ids = append(ids, row.ID)
	}
	if err := ctx.Err(); err != nil {
		return fail(total, err)
	}
	if err := idx.DeleteEntryIDs(ids); err != nil {
		return fail(total, err)
	}
	reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildDelete, Total: total})
	n, err := indexVecRebuildRows(ctx, idx, selected, onProgress)
	if err != nil {
		return n, err
	}
	reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildDone, Total: total, Indexed: n})
	return n, nil
}

// indexVecRebuildRows 写入选中的记忆，并按间隔汇报进度。
// idx 为空时仍计数（测试不必挂上 sqlite-vec）。ctx 取消会停在下一条之前。
func indexVecRebuildRows(ctx context.Context, idx *VecIndex, selected []dao.TieredMemoryModel, onProgress func(VecRebuildProgress)) (int, error) {
	total := len(selected)
	n := 0
	since := 0
	for i, row := range selected {
		if err := ctx.Err(); err != nil {
			reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildError, Total: total, Indexed: n})
			return n, err
		}
		if row.ID != "" && row.Content != "" {
			if idx != nil {
				entry := modelToTiedEntry(row)
				idx.Upsert(VectorScopeKey(entry.Scope, entry.Metadata), row.ID, row.Content)
			}
			n++
			since++
		}
		if n > 0 && (since >= vecRebuildProgressEvery || i == len(selected)-1) {
			reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildWrite, Total: total, Indexed: n})
			since = 0
		}
	}
	return n, nil
}

// RebuildVec 用这个 store 已打开的库重建该 bot 的向量。
func (s *TieredStore) RebuildVec(ctx context.Context, botID string) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("sqlite-vec is not available")
	}
	return RebuildBotVectors(ctx, s.db, botID, s, nil)
}

func ensureVecIndex(gdb *gorm.DB, live *TieredStore) (*VecIndex, error) {
	if live != nil && live.Vec().Enabled() {
		return live.Vec(), nil
	}
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		return nil, fmt.Errorf("sqlite-vec is not available")
	}
	if live != nil {
		live.mu.Lock()
		live.vec = idx
		live.mu.Unlock()
	}
	return idx, nil
}

// selectVecRebuildRows uses the same explicit ownership rule as bot-bound
// repositories. A historical channel/user association never assigns old data.
func selectVecRebuildRows(botID string, rows []dao.TieredMemoryModel) []dao.TieredMemoryModel {
	var selected []dao.TieredMemoryModel
	if botID == "" {
		return selected
	}
	for _, row := range rows {
		var metadata map[string]any
		_ = json.Unmarshal([]byte(row.MetadataJSON), &metadata)
		owner := metadata["bot_id"]
		if row.ScopeKind == string(ScopeBot) {
			if row.ScopeID != botID || (owner != nil && owner != botID) {
				continue
			}
		} else if owner != botID {
			continue
		}
		selected = append(selected, row)
	}
	return selected
}
