package secret

import (
	"strings"

	ulog "github.com/kasuganosora/thinkbot/util/log"
)

// 删除结果的归属范围。
const (
	ScopeBot    = "bot"
	ScopeGlobal = "global"
)

// DeleteStatus 是删除凭据的结果状态。
type DeleteStatus string

const (
	// DeleteStatusDeleted 条目已从内存与持久化文件移除。
	DeleteStatusDeleted DeleteStatus = "deleted"
	// DeleteStatusConfirmRequired 目标是一条全局共享条目，需显式确认才能删。
	DeleteStatusConfirmRequired DeleteStatus = "confirm_required"
	// DeleteStatusNotFound 本 bot 可见范围内没有这个名字。
	DeleteStatusNotFound DeleteStatus = "not_found"
)

// DeleteResult 给工具的返回体：只有名字 / 归属 / 状态，绝不含明文值。
type DeleteResult struct {
	Status DeleteStatus `json:"status"`
	Name   string       `json:"name"`
	Scope  string       `json:"scope,omitempty"`
	// RevealedGlobal 表示删掉本 bot 的覆盖条目后，同名的全局条目会重新可见。
	RevealedGlobal bool `json:"revealed_global,omitempty"`
}

// Delete 按「对 botID 可见」的语义删除一条凭据，与 Get / Catalog 的解析顺序
// 保持一致：先找本 bot 名下的条目，找不到再回落全局条目。
//
// 全局条目是早期登记的遗留项，所有 bot 都能看见，删除会影响其它 bot，因此
// 默认不删：返回 ConfirmRequired，由调用方拿到用户确认后带 allowGlobal=true
// 再调一次。botID 为空时唯一可见的就是全局条目，allowGlobal 同样生效。
//
// 与 Register 一样，删内存与落盘在 registerMu 下作为一个事务：落盘失败会
// 把条目放回内存，保证「返回错误 = 没删掉」。删除成功后同步释放该条目在
// 日志遮蔽层的那一份引用（短值尤其不能留着继续打码）；同值若还被其它条目
// 或渠道配置持有，遮蔽规则由剩余引用继续保住。
func (v *Vault) Delete(botID, name string, allowGlobal bool) (DeleteResult, error) {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return DeleteResult{}, ErrInvalidName
	}

	v.registerMu.Lock()
	defer v.registerMu.Unlock()
	if v.loadErr != nil {
		return DeleteResult{}, ErrUnavailable
	}

	key := entryKey{botID, name}
	scope := ScopeBot
	targetIsGlobal := botID == ""
	if targetIsGlobal {
		scope = ScopeGlobal
	}
	v.mu.RLock()
	entry, ok := v.entries[key]
	if !ok && botID != "" {
		globalKey := entryKey{"", name}
		if g, found := v.entries[globalKey]; found {
			if !allowGlobal {
				v.mu.RUnlock()
				return DeleteResult{
					Status: DeleteStatusConfirmRequired,
					Name:   name,
					Scope:  ScopeGlobal,
				}, nil
			}
			key, entry, ok, scope, targetIsGlobal = globalKey, g, true, ScopeGlobal, true
		}
	}
	v.mu.RUnlock()
	if !ok {
		return DeleteResult{Status: DeleteStatusNotFound, Name: name}, nil
	}

	v.mu.Lock()
	delete(v.entries, key)
	result := DeleteResult{Status: DeleteStatusDeleted, Name: name, Scope: scope}
	if !targetIsGlobal && botID != "" {
		_, result.RevealedGlobal = v.entries[entryKey{"", name}]
	}
	v.mu.Unlock()

	if err := v.save(); err != nil {
		v.mu.Lock()
		v.entries[key] = entry
		v.mu.Unlock()
		return DeleteResult{}, err // 条目还在，引用不动
	}
	// 释放该条目当初登记的那一份引用；同值仍被其它条目 / 渠道配置持有时，
	// 日志遮蔽规则由剩余引用继续保住。
	ulog.UnregisterSecret(entry.Value)
	return result, nil
}
