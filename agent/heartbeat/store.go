package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ============================================================================
// Store — 心跳配置和日志的文件系统存储
//
// 存储结构：
//   data/heartbeat/{botId}/config.json   — 心跳配置
//   data/heartbeat/{botId}/logs.json     — 心跳日志（滚动窗口）
//   data/heartbeat/{botId}/.cron.json    — cron 调度器状态
// ============================================================================

const (
	// MaxLogEntries 最多保留的日志条数。
	MaxLogEntries = 200
)

// LogStore 日志存储结构。
type LogStore struct {
	Logs  []Log `json:"logs"`
	Total int   `json:"total"` // 历史总数（含已滚出的）
}

// Store 管理心跳数据的文件读写。
// 线程安全（通过 per-bot 锁保护）。
type Store struct {
	dataDir string
	mu      sync.Map // map[botID]*sync.Mutex
}

// NewStore 创建心跳存储。
func NewStore(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

// botMu 获取 bot 级别的互斥锁。
func (s *Store) botMu(botID string) *sync.Mutex {
	v, _ := s.mu.LoadOrStore(botID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// botDir 返回 bot 的心跳数据目录。
func (s *Store) botDir(botID string) string {
	return filepath.Join(s.dataDir, botID)
}

// configPath 返回配置文件路径。
func (s *Store) configPath(botID string) string {
	return filepath.Join(s.botDir(botID), "config.json")
}

// logsPath 返回日志文件路径。
func (s *Store) logsPath(botID string) string {
	return filepath.Join(s.botDir(botID), "logs.json")
}

// CronFilePath 返回 cron 调度器的持久化文件路径。
func (s *Store) CronFilePath(botID string) string {
	return filepath.Join(s.botDir(botID), ".cron.json")
}

// LoadConfig 加载心跳配置。
// 文件不存在时返回 nil, nil。
func (s *Store) LoadConfig(botID string) (*Config, error) {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(s.configPath(botID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig 保存心跳配置。
func (s *Store) SaveConfig(botID string, cfg *Config) error {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	return s.writeJSON(s.configPath(botID), cfg)
}

// LoadLogs 加载心跳日志。
// 文件不存在时返回空 LogStore。
func (s *Store) LoadLogs(botID string) (*LogStore, error) {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	return s.loadLogsUnsafe(botID)
}

// loadLogsUnsafe 无锁版本，供内部使用。
func (s *Store) loadLogsUnsafe(botID string) (*LogStore, error) {
	data, err := os.ReadFile(s.logsPath(botID))
	if err != nil {
		if os.IsNotExist(err) {
			return &LogStore{Logs: []Log{}, Total: 0}, nil
		}
		return nil, err
	}
	var store LogStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, err
	}
	if store.Logs == nil {
		store.Logs = []Log{}
	}
	return &store, nil
}

// AppendLog 追加一条心跳日志。
// 自动滚动：超过 MaxLogEntries 时删除最老的条目。
// 新日志始终插入到头部（最新在前）。
func (s *Store) AppendLog(botID string, entry Log) error {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	store, err := s.loadLogsUnsafe(botID)
	if err != nil {
		store = &LogStore{Logs: []Log{}, Total: 0}
	}

	// 头部插入
	store.Logs = append([]Log{entry}, store.Logs...)
	store.Total++

	// 滚动截断
	if len(store.Logs) > MaxLogEntries {
		store.Logs = store.Logs[:MaxLogEntries]
	}

	return s.writeJSON(s.logsPath(botID), store)
}

// ClearLogs 清空心跳日志。
func (s *Store) ClearLogs(botID string) error {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	store := &LogStore{Logs: []Log{}, Total: 0}
	return s.writeJSON(s.logsPath(botID), store)
}

// writeJSON 将数据序列化为 JSON 并写入文件。
func (s *Store) writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
