package sandbox

import (
	"path"
	"sort"
	"strings"
	"sync"
)

// workspacePathLocks 按 bot + 规范化路径串行化写工具。
// 并行工作流节点共用一个工作空间。replace 是先读后写，两个人同时改不同的行
// 会各自基于旧内容写回，后写的那次把先写的改动盖掉。
// 写、删、移动和替换都走这里。不同路径互不阻塞；移动同时锁住源和目标。
var workspacePathLocks sync.Map // key string -> *sync.Mutex

func canonicalWorkspacePath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func workspacePathLockKey(botID, p string) string {
	return botID + "\x00" + canonicalWorkspacePath(p)
}

func withWorkspacePathLock(botID, rel string, fn func() (any, error)) (any, error) {
	return withWorkspacePathLocks(botID, []string{rel}, fn)
}

// withWorkspacePathLocks 按键排序后加锁，避免 A→B 与 B→A 互相等待。
func withWorkspacePathLocks(botID string, rels []string, fn func() (any, error)) (any, error) {
	seen := make(map[string]struct{}, len(rels))
	keys := make([]string, 0, len(rels))
	for _, rel := range rels {
		k := workspacePathLockKey(botID, rel)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mus := make([]*sync.Mutex, len(keys))
	for i, k := range keys {
		v, _ := workspacePathLocks.LoadOrStore(k, &sync.Mutex{})
		mu := v.(*sync.Mutex)
		mu.Lock()
		mus[i] = mu
	}
	defer func() {
		for i := len(mus) - 1; i >= 0; i-- {
			mus[i].Unlock()
		}
	}()
	return fn()
}
