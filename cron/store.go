package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ============================================================================
// Store — Job 的 JSON 文件存储
//
// 设计要点：
//   - 每个 bot 一个 JSON 文件（{dir}/{botID}_cron.json）
//   - 原子写入（写临时文件 → rename）
//   - 读写锁保护内存中的 map
//   - 所有 NextRunAt/LastRunAt 以 UTC 时间戳存储
// ============================================================================

// Store 提供 Job 的持久化存储。
type Store struct {
	mu       sync.RWMutex
	filePath string
	jobs     map[string]*Job // id → job
	loaded   bool
}

// NewStore 创建一个 JSON 文件存储。
// filePath 是存储文件的完整路径。
func NewStore(filePath string) *Store {
	return &Store{
		filePath: filePath,
		jobs:     make(map[string]*Job),
	}
}

// load 从磁盘加载所有 Job（首次调用时延迟加载）。
func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.loaded {
		return nil
	}
	s.loaded = true

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在是正常状态
		}
		return err
	}

	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return err
	}

	for _, j := range jobs {
		s.jobs[j.ID] = j
	}
	return nil
}

// save 原子写入所有 Job 到磁盘。
func (s *Store) save() error {
	jobs := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	// 按 ID 排序确保文件稳定
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].ID < jobs[j].ID
	})

	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}

	// 确保目录存在
	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	// 原子写入：临时文件 → rename
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

// Get 返回指定 ID 的 Job 副本。
func (s *Store) Get(id string) (*Job, bool) {
	_ = s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// List 返回所有 Job 的副本列表。
func (s *Store) List() []*Job {
	_ = s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		cp := *j
		result = append(result, &cp)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// ListActive 返回所有活跃状态（非 Done/Failed/Disabled/Paused）的 Job。
func (s *Store) ListActive() []*Job {
	_ = s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Job, 0)
	for _, j := range s.jobs {
		if !j.Enabled || j.State != StateActive {
			continue
		}
		cp := *j
		result = append(result, &cp)
	}
	return result
}

// Save 保存或更新一个 Job（写入内存 + 持久化）。
func (s *Store) Save(j *Job) error {
	_ = s.load()
	s.mu.Lock()
	j.UpdatedAt = time.Now()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	return s.save()
}

// Delete 删除一个 Job。不存在时静默忽略。
func (s *Store) Delete(id string) error {
	_ = s.load()
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
	return s.save()
}
