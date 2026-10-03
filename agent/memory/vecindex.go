package memory

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
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
func OpenVecIndex(gdb *gorm.DB) *VecIndex {
	if gdb == nil || !db.TryEnableVec(gdb) {
		return nil
	}
	idx := &VecIndex{db: gdb}
	err := gdb.Exec(vecTableDDL).Error
	if err != nil {
		return nil
	}
	idx.ready = true
	return idx
}

func (v *VecIndex) Enabled() bool { return v != nil && v.ready }

const vecTableDDL = `CREATE VIRTUAL TABLE IF NOT EXISTS memory_vec USING vec0(
		embedding float[256],
		+scope_key text,
		+entry_id text
	)`

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
	if err := v.db.Exec(vecTableDDL).Error; err != nil {
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
		return nil
	}
	out := make([]Hit, 0, len(rows))
	for _, row := range rows {
		out = append(out, Hit{EntryID: row.EntryID, Distance: row.Distance})
	}
	return out
}

// hashEmbed 是无外部模型时的本地向量：字符二元组哈希到 256 维后归一化。
// 有 sqlite-vec 就能用；配了外部 embedding 服务后可替换这一层，表结构不变。
func hashEmbed(text string) []byte {
	vec := make([]float32, vecDims)
	runes := []rune(strings.ToLower(text))
	if len(runes) == 0 {
		return encodeVec(vec)
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
	return encodeVec(vec)
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
	raw := hashEmbed(text)
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
