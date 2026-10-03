package memory

import (
	"encoding/binary"
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
	err := gdb.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memory_vec USING vec0(
		embedding float[256],
		+scope_key text,
		+entry_id text
	)`).Error
	if err != nil {
		return nil
	}
	idx.ready = true
	return idx
}

func (v *VecIndex) Enabled() bool { return v != nil && v.ready }

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
