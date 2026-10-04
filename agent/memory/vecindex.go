package memory

import (
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"strings"
	"sync"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/db"
)

const vecDims = 256

// VecIndex 是可选的 sqlite-vec 索引。未启用时为 nil，检索退回 scope 子串。
type VecIndex struct {
	db    *gorm.DB
	mu    sync.Mutex
	ready bool
}

// OpenVecIndex 在 sqlite-vec 可用时建表。不可用返回 nil。
// 已有的 memory_vec 若仍把 scope_key 存成辅助列，会在这里就地迁到 metadata 列。
func OpenVecIndex(gdb *gorm.DB) *VecIndex {
	if gdb == nil || !db.TryEnableVec(gdb) {
		return nil
	}
	idx := &VecIndex{db: gdb}
	if err := idx.ensureVecSchema(); err != nil {
		log.Printf("memory vec schema: %v", err)
		return nil
	}
	idx.ready = true
	return idx
}

func (v *VecIndex) Enabled() bool { return v != nil && v.ready }

const (
	vecTableName    = "memory_vec"
	vecMigrateTable = "memory_vec_migrating"
)

// vecSchemaMu 串行化建表和迁移。进程里所有 bot 共用一张 memory_vec。
var vecSchemaMu sync.Mutex

// scope_key 不加 "+"，是 sqlite-vec 的 metadata 列，KNN 的 WHERE 可以等值过滤。
// "+" 辅助列会报 "illegal WHERE constraint"。entry_id 只按 id 删除，保持辅助列。
func vecCreateSQL(table string, ifNotExists bool) string {
	opt := ""
	if ifNotExists {
		opt = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE VIRTUAL TABLE %s%s USING vec0(
		embedding float[%d],
		scope_key text,
		+entry_id text
	)`, opt, table, vecDims)
}

// vecCreateSQLAux 是迁移前的旧表结构。复制失败时用它把 memory_vec 原样放回去。
func vecCreateSQLAux(table string) string {
	return fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(
		embedding float[%d],
		+scope_key text,
		+entry_id text
	)`, table, vecDims)
}

func scopeKeyAuxiliary(createSQL string) bool {
	return strings.Contains(createSQL, "+scope_key")
}

func lookupCreateSQL(gdb *gorm.DB, name string) (string, bool, error) {
	var createSQL sql.NullString
	err := gdb.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", name).Row().Scan(&createSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !createSQL.Valid {
		return "", true, nil
	}
	return createSQL.String, true, nil
}

func (v *VecIndex) ensureVecSchema() error {
	vecSchemaMu.Lock()
	defer vecSchemaMu.Unlock()

	liveSQL, liveExists, err := lookupCreateSQL(v.db, vecTableName)
	if err != nil {
		return err
	}
	_, sideExists, err := lookupCreateSQL(v.db, vecMigrateTable)
	if err != nil {
		return err
	}
	if !liveExists && !sideExists {
		return v.db.Exec(vecCreateSQL(vecTableName, true)).Error
	}
	if !liveExists && sideExists {
		// 上次在删掉旧表之后中断。边表里是已经复制好的新结构，把它装回 memory_vec。
		return v.finishVecSwap()
	}
	if !scopeKeyAuxiliary(liveSQL) {
		if sideExists {
			if err := v.db.Exec("DROP TABLE " + vecMigrateTable).Error; err != nil {
				log.Printf("memory vec: drop leftover %s: %v", vecMigrateTable, err)
			}
		}
		return nil
	}
	if sideExists {
		if err := v.db.Exec("DROP TABLE " + vecMigrateTable).Error; err != nil {
			return fmt.Errorf("drop leftover %s: %w", vecMigrateTable, err)
		}
	}
	if err := v.db.Exec(vecCreateSQL(vecMigrateTable, false)).Error; err != nil {
		return err
	}
	copied, err := v.copyVec(vecMigrateTable, vecTableName)
	if err != nil {
		if dropErr := v.db.Exec("DROP TABLE " + vecMigrateTable).Error; dropErr != nil {
			return fmt.Errorf("copy memory_vec: %w (drop %s: %v)", err, vecMigrateTable, dropErr)
		}
		return fmt.Errorf("copy memory_vec: %w", err)
	}
	if err := v.db.Exec("DROP TABLE " + vecTableName).Error; err != nil {
		_ = v.db.Exec("DROP TABLE " + vecMigrateTable).Error
		return fmt.Errorf("drop old memory_vec: %w", err)
	}
	if err := v.finishVecSwap(); err != nil {
		return err
	}
	log.Printf("memory vec: migrated scope_key to a metadata column (%d rows)", copied)
	return nil
}

// copyVec 把 src 的向量行写入已经建好的 dst。行数不一致视为失败，调用方决定删哪张表。
func (v *VecIndex) copyVec(dst, src string) (int, error) {
	if err := v.db.Exec(fmt.Sprintf(
		"INSERT INTO %s(embedding, scope_key, entry_id) SELECT embedding, scope_key, entry_id FROM %s",
		dst, src)).Error; err != nil {
		return 0, err
	}
	srcN, err := v.vecCount(src)
	if err != nil {
		return 0, err
	}
	dstN, err := v.vecCount(dst)
	if err != nil {
		return 0, err
	}
	if srcN != dstN {
		return dstN, fmt.Errorf("copied %d of %d rows", dstN, srcN)
	}
	return dstN, nil
}

func (v *VecIndex) vecCount(table string) (int, error) {
	var n int
	err := v.db.Raw("SELECT count(*) FROM " + table).Scan(&n).Error
	return n, err
}

// finishVecSwap 在 memory_vec 已经不在、边表持有全部行时，按新结构建回 memory_vec。
// 建表或复制失败会从边表恢复旧的辅助列结构，避免最后一张表都没有。
func (v *VecIndex) finishVecSwap() error {
	if err := v.db.Exec(vecCreateSQL(vecTableName, false)).Error; err != nil {
		return v.restoreAuxTable(err)
	}
	if _, err := v.copyVec(vecTableName, vecMigrateTable); err != nil {
		if dropErr := v.db.Exec("DROP TABLE " + vecTableName).Error; dropErr != nil {
			return fmt.Errorf("copy memory_vec back: %w (drop partial: %v)", err, dropErr)
		}
		return v.restoreAuxTable(fmt.Errorf("copy memory_vec back: %w", err))
	}
	if err := v.db.Exec("DROP TABLE " + vecMigrateTable).Error; err != nil {
		log.Printf("memory vec: migrated but failed to drop %s: %v", vecMigrateTable, err)
	}
	return nil
}

func (v *VecIndex) restoreAuxTable(cause error) error {
	liveSQL, liveExists, err := lookupCreateSQL(v.db, vecTableName)
	if err != nil {
		return fmt.Errorf("%w (schema lookup: %v)", cause, err)
	}
	if liveExists && !scopeKeyAuxiliary(liveSQL) {
		if err := v.db.Exec("DROP TABLE " + vecTableName).Error; err != nil {
			return fmt.Errorf("%w (drop partial memory_vec: %v)", cause, err)
		}
		liveExists = false
	}
	if liveExists {
		_ = v.db.Exec("DROP TABLE " + vecMigrateTable).Error
		return cause
	}
	if err := v.db.Exec(vecCreateSQLAux(vecTableName)).Error; err != nil {
		return fmt.Errorf("%w (recreate auxiliary memory_vec: %v)", cause, err)
	}
	if _, err := v.copyVec(vecTableName, vecMigrateTable); err != nil {
		return fmt.Errorf("%w (restore rows: %v)", cause, err)
	}
	if err := v.db.Exec("DROP TABLE " + vecMigrateTable).Error; err != nil {
		return fmt.Errorf("%w (restored auxiliary table, drop %s: %v)", cause, vecMigrateTable, err)
	}
	return cause
}

// Rebuild 丢掉并重建整张 memory_vec。只在这份 SQLite 只属于一个 bot 时安全。
// 进程共用一个库时必须走按 scope/entry 删除（见 RebuildBotVectors），否则会清掉其他 bot 的向量。
func (v *VecIndex) Rebuild() error {
	if !v.Enabled() {
		return fmt.Errorf("sqlite-vec is not available")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.db.Exec("DROP TABLE IF EXISTS memory_vec").Error; err != nil {
		return err
	}
	if err := v.db.Exec(vecCreateSQL(vecTableName, true)).Error; err != nil {
		v.ready = false
		return err
	}
	v.ready = true
	return nil
}

// DeleteScopes 只删这些 scope_key 的向量，不动其他 bot 的行。
func (v *VecIndex) DeleteScopes(scopeKeys []string) error {
	if !v.Enabled() || len(scopeKeys) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, key := range scopeKeys {
		if key == "" {
			continue
		}
		if err := v.db.Exec("DELETE FROM memory_vec WHERE scope_key = ?", key).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteEntryIDs 按 entry_id 删除。共享 scope 里只换属于本 bot 的行时用这个。
func (v *VecIndex) DeleteEntryIDs(ids []string) error {
	if !v.Enabled() || len(ids) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	const chunk = 200
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[i:end]
		args := make([]any, len(part))
		holders := make([]string, len(part))
		for j, id := range part {
			args[j] = id
			holders[j] = "?"
		}
		q := "DELETE FROM memory_vec WHERE entry_id IN (" + strings.Join(holders, ",") + ")"
		if err := v.db.Exec(q, args...).Error; err != nil {
			return err
		}
	}
	return nil
}

// Upsert 写入一条记忆的向量。失败只表示这条不进索引，不阻断记忆本身。
func (v *VecIndex) Upsert(scopeKey, entryID, content string) {
	if !v.Enabled() || entryID == "" || content == "" {
		return
	}
	blob := hashEmbed(content)
	v.mu.Lock()
	defer v.mu.Unlock()
	_ = v.db.Exec("DELETE FROM memory_vec WHERE entry_id = ?", entryID).Error
	_ = v.db.Exec("INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)", blob, scopeKey, entryID).Error
}

func (v *VecIndex) Delete(entryID string) {
	if !v.Enabled() || entryID == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	_ = v.db.Exec("DELETE FROM memory_vec WHERE entry_id = ?", entryID).Error
}

// Hit 是一次向量近邻。
type Hit struct {
	EntryID  string
	Distance float64
}

// Search 在一个 scope 内做近邻检索。扩展不可用或查询失败返回 nil。
func (v *VecIndex) Search(scopeKey, query string, k int) []Hit {
	if !v.Enabled() || query == "" {
		return nil
	}
	if k <= 0 {
		k = 8
	}
	var rows []struct {
		EntryID  string  `gorm:"column:entry_id"`
		Distance float64 `gorm:"column:distance"`
	}
	err := v.db.Raw(`SELECT entry_id, distance FROM memory_vec
		WHERE embedding MATCH ? AND k = ? AND scope_key = ?`, hashEmbed(query), k, scopeKey).Scan(&rows).Error
	if err != nil {
		log.Printf("memory vec search failed scope_key=%q: %v", scopeKey, err)
		return nil
	}
	out := make([]Hit, 0, len(rows))
	for _, row := range rows {
		out = append(out, Hit{EntryID: row.EntryID, Distance: row.Distance})
	}
	return out
}

// vecBlob 是一段向量字节。GORM 在占位符紧挨 '(' 时会把 []byte 按切片展开，
// 1024 字节会变成 1024 个参数（再加上 scope、entry 就是 1026 values for 3 columns）。
// driver.Valuer 会跳过这次展开，整段仍是一个绑定参数。
type vecBlob []byte

func (b vecBlob) Value() (driver.Value, error) {
	return []byte(b), nil
}

// hashEmbed 是无外部模型时的本地向量：字符二元组哈希到 256 维后归一化。
// 有 sqlite-vec 就能用；配了外部 embedding 服务后可替换这一层，表结构不变。
// 返回 vecBlob，避免 INSERT/MATCH 被 GORM 拆成逐字节参数。
func hashEmbed(text string) vecBlob {
	vec := make([]float32, vecDims)
	runes := []rune(strings.ToLower(text))
	if len(runes) == 0 {
		return vecBlob(encodeVec(vec))
	}
	add := func(s string) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(s))
		vec[h.Sum32()%vecDims]++
	}
	for i, r := range runes {
		add(string(r))
		if i+1 < len(runes) {
			add(string(runes[i : i+2]))
		}
	}
	var norm float64
	for _, x := range vec {
		norm += float64(x) * float64(x)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vecBlob(encodeVec(vec))
}

// EmbedSimilarity 是写入 sqlite-vec 的同一套向量的余弦相似度。没有扩展时也能用来挡无关记忆。
func EmbedSimilarity(a, b string) float64 {
	av := hashVec(a)
	bv := hashVec(b)
	var dot float64
	for i := range av {
		dot += float64(av[i]) * float64(bv[i])
	}
	return dot
}

func hashVec(text string) []float32 {
	raw := []byte(hashEmbed(text))
	out := make([]float32, vecDims)
	for i := 0; i < vecDims; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

func encodeVec(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, x := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}
