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
// 因此这里不会 DROP 整表，只删除本 bot 的 scope_key / entry_id 再插入。
//
// 纳入规则：
//   - scope bot:<botID> 整桶（bot 专属）
//   - metadata.bot_id 等于该 bot 的行
//   - 某个 channel/user scope 只出现在该 bot 的 user_message_events 里，
//     且桶内没有其他 bot 的 metadata.bot_id：整桶重建（覆盖没打 bot_id 的梦境产物）
//   - 同一 scope 里混有其他 bot 的标记：只替换 metadata.bot_id 属于本 bot 的行
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
	var events []dao.UserMessageEvent
	if err := gdb.WithContext(ctx).Select("bot_id", "channel", "user_id").Find(&events).Error; err != nil {
		return fail(0, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(0, err)
	}
	selected, fullKeys, partialIDs := selectVecRebuildRows(botID, rows, events)
	total := len(selected)
	reportVecRebuild(onProgress, VecRebuildProgress{Phase: VecRebuildSelect, Total: total})
	if err := idx.DeleteScopes(fullKeys); err != nil {
		return fail(total, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(total, err)
	}
	if err := idx.DeleteEntryIDs(partialIDs); err != nil {
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
				idx.Upsert(scopeKeyOf(row.ScopeKind, row.ScopeID), row.ID, row.Content)
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

func scopeKeyOf(kind, id string) string {
	if id == "" {
		return kind
	}
	return kind + ":" + id
}

func metadataBotID(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	s, _ := m["bot_id"].(string)
	return s
}

type vecRebuildGroup struct {
	rows []dao.TieredMemoryModel
}

// selectVecRebuildRows 决定哪些记忆行属于这个 bot，以及删除时用整桶还是按 entry。
func selectVecRebuildRows(botID string, rows []dao.TieredMemoryModel, events []dao.UserMessageEvent) (selected []dao.TieredMemoryModel, fullKeys []string, partialIDs []string) {
	thisCh := map[string]struct{}{}
	otherCh := map[string]struct{}{}
	thisUser := map[string]struct{}{}
	otherUser := map[string]struct{}{}
	for _, e := range events {
		ch, user := thisCh, thisUser
		if e.BotID != botID {
			ch, user = otherCh, otherUser
		}
		if e.Channel != "" {
			ch[e.Channel] = struct{}{}
		}
		if e.UserID != "" {
			user[e.UserID] = struct{}{}
		}
	}
	groups := map[string]*vecRebuildGroup{}
	order := make([]string, 0)
	for _, row := range rows {
		key := scopeKeyOf(row.ScopeKind, row.ScopeID)
		g := groups[key]
		if g == nil {
			g = &vecRebuildGroup{}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, row)
	}
	botKey := scopeKeyOf(string(ScopeBot), botID)
	for _, key := range order {
		g := groups[key]
		if key == botKey {
			fullKeys = append(fullKeys, key)
			selected = append(selected, g.rows...)
			continue
		}
		taggedThis := 0
		taggedOther := 0
		for _, row := range g.rows {
			bid := metadataBotID(row.MetadataJSON)
			switch {
			case bid == "":
			case bid == botID:
				taggedThis++
			default:
				taggedOther++
			}
		}
		if taggedOther > 0 {
			for _, row := range g.rows {
				if metadataBotID(row.MetadataJSON) == botID {
					selected = append(selected, row)
					if row.ID != "" {
						partialIDs = append(partialIDs, row.ID)
					}
				}
			}
			continue
		}
		exclusive := scopeExclusive(key, thisCh, otherCh, thisUser, otherUser)
		if exclusive || taggedThis > 0 {
			fullKeys = append(fullKeys, key)
			selected = append(selected, g.rows...)
		}
	}
	return selected, fullKeys, partialIDs
}

func scopeExclusive(key string, thisCh, otherCh, thisUser, otherUser map[string]struct{}) bool {
	const chP, userP = "channel:", "user:"
	switch {
	case len(key) > len(chP) && key[:len(chP)] == chP:
		id := key[len(chP):]
		_, mine := thisCh[id]
		_, other := otherCh[id]
		return mine && !other
	case len(key) > len(userP) && key[:len(userP)] == userP:
		id := key[len(userP):]
		_, mine := thisUser[id]
		_, other := otherUser[id]
		return mine && !other
	default:
		return false
	}
}
