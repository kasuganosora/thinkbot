package outreach

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ConfigStore 心跳同构的 per-bot JSON 配置 + cron 文件路径。
//
//	data/outreach/{botId}/config.json
//	data/outreach/{botId}/.cron.json
type ConfigStore struct {
	dataDir string
	mu      sync.Map // map[botID]*sync.Mutex
}

// NewConfigStore 创建配置存储。dataDir 默认 "data/outreach"。
func NewConfigStore(dataDir string) *ConfigStore {
	if dataDir == "" {
		dataDir = "data/outreach"
	}
	return &ConfigStore{dataDir: dataDir}
}

func (s *ConfigStore) botMu(botID string) *sync.Mutex {
	v, _ := s.mu.LoadOrStore(botID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *ConfigStore) botDir(botID string) string {
	return filepath.Join(s.dataDir, botID)
}

func (s *ConfigStore) configPath(botID string) string {
	return filepath.Join(s.botDir(botID), "config.json")
}

// CronFilePath 返回该 bot 的 outreach cron 持久化文件。
func (s *ConfigStore) CronFilePath(botID string) string {
	return filepath.Join(s.botDir(botID), ".cron.json")
}

// Load 加载配置。文件不存在时返回 DefaultConfig。
func (s *ConfigStore) Load(botID string) (Config, error) {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(s.configPath(botID))
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			return cfg, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.Normalize()
	return cfg, nil
}

// Save 保存配置。
func (s *ConfigStore) Save(botID string, cfg Config) error {
	mu := s.botMu(botID)
	mu.Lock()
	defer mu.Unlock()

	cfg.Normalize()
	dir := s.botDir(botID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.configPath(botID), data, 0o644)
}
