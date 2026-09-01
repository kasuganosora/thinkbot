package searchproviders

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultFile 是搜索提供方配置的默认相对路径，相对进程工作目录。
const DefaultFile = "data/search/providers.json"

// Store 读写 providers.json。API 与 web_search 共用同一份文件，
// 所以工具层每次调用时都能看到 Web UI 刚改过的提供方。
type Store struct {
	Path string
	mu   sync.Mutex
}

var defaultStore = NewStore(DefaultFile)

// DefaultStore 返回指向 data/search/providers.json 的全局存储。
func DefaultStore() *Store { return defaultStore }

// NewStore 创建指向自定义路径的存储（测试用）。
func NewStore(path string) *Store {
	if path == "" {
		path = DefaultFile
	}
	return &Store{Path: path}
}

// List 读取全部提供方；文件不存在时返回空列表而非错误。
func (s *Store) List() ([]Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) listLocked() ([]Provider, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var providers []Provider
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("parse search providers: %w", err)
	}
	return providers, nil
}

// Save 覆盖写入全部提供方。成功后清空鉴权/限流熔断（UI 更新了 key）。
func (s *Store) Save(providers []Provider) error {
	s.mu.Lock()
	err := s.saveLocked(providers)
	s.mu.Unlock()
	if err == nil {
		clearCircuits()
	}
	return err
}

func (s *Store) saveLocked(providers []Provider) error {
	if providers == nil {
		providers = []Provider{}
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.Path, data, 0o644)
}

// EnabledList 返回所有已启用的提供方，顺序与文件一致（UI 顺序即优先级）。
func (s *Store) EnabledList() ([]Provider, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(list))
	for i := range list {
		if list[i].Enabled {
			out = append(out, list[i])
		}
	}
	return out, nil
}

// Enabled 返回第一个已启用的提供方。
// 没有启用项时返回明确错误，而不是悄悄回退到 Instant Answer。
func (s *Store) Enabled() (*Provider, error) {
	list, err := s.EnabledList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no search provider is enabled; enable one in Settings → Search Providers")
	}
	p := list[0]
	return &p, nil
}
