package dao

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// DataMigration 记录一次性数据修复（与表结构迁移区分）是否已执行。
//
// 表结构迁移（AutoMigrate / ensureColumns）天然幂等，可每次启动重跑；数据修复
// 不是——例如把 cost_total 按比例扣减，重复执行会越扣越少。因此每个数据修复
// 以唯一 Name 登记，执行与登记在同一事务内完成，保证「恰好一次」。
type DataMigration struct {
	Name      string    `gorm:"primaryKey;size:128" json:"name"`
	AppliedAt time.Time `json:"appliedAt"`
	Detail    string    `gorm:"type:text" json:"detail"`
}

// TableName 指定表名。
func (DataMigration) TableName() string { return "data_migrations" }

// dataMigration 描述一个一次性数据修复。
type dataMigration struct {
	name string
	// run 在事务内执行修复，返回写入登记表的说明文本。
	run func(tx *gorm.DB) (string, error)
}

// dataMigrations 按顺序执行的数据修复列表（只追加，不要改名 / 删除已发布的项）。
var dataMigrations = []dataMigration{
	{name: costCacheDoubleCountFixName, run: fixCostCacheDoubleCount},
}

// runDataMigrations 依次执行尚未登记的数据修复。
//
// 必须在任何写 stats_usage_daily 的组件（stats.Recorder）启动之前执行：
// 当日聚合行是 UPSERT 累加的，若新代码先写入正确花费、再对整行做修复，
// 新写入的那部分也会被错误扣减。dao.Migrate 在 fx.Invoke 阶段运行，
// 早于各模块 OnStart，满足此前提。
func runDataMigrations(db *gorm.DB) error {
	if err := db.AutoMigrate(&DataMigration{}); err != nil {
		return err
	}
	for _, m := range dataMigrations {
		err := db.Transaction(func(tx *gorm.DB) error {
			var cnt int64
			if err := tx.Model(&DataMigration{}).Where("name = ?", m.name).Count(&cnt).Error; err != nil {
				return err
			}
			if cnt > 0 {
				return nil
			}
			detail, err := m.run(tx)
			if err != nil {
				return err
			}
			return tx.Create(&DataMigration{Name: m.name, AppliedAt: time.Now().UTC(), Detail: detail}).Error
		})
		if err != nil {
			return fmt.Errorf("data migration %s: %w", m.name, err)
		}
	}
	return nil
}

// costCacheDoubleCountFixName 修正「缓存命中 token 双计」写入的 cost_* 列。
const costCacheDoubleCountFixName = "2026-09-26-cost-cache-double-count"

// fixCostCacheDoubleCount 修正旧计费公式落库的花费。
//
// 旧公式：cost_input = in*Pin，cost_total = in*Pin + out*Pout + cacheRead*Pcache，
// 其中 in（input_tokens）已包含 cacheRead，命中部分被全价 + 缓存价各算一次。
// 新公式：cost_input = (in-cacheRead)*Pin + cacheRead*Pcache，cost_total = cost_input + cost_output。
//
// 修正只用行内已有的量，不依赖「当前」单价（历史单价以落库值为准）：
//
//	Pin 部分的多算 = cost_input * cacheRead / in
//	旧缓存项       = cost_total - cost_input - cost_output
//	新 cost_input  = cost_input - 多算 + 旧缓存项
//	新 cost_total  = cost_total - 多算
//
// SQLite 的 UPDATE 右侧表达式一律引用更新前的旧值，所以两列可在同一语句里计算。
// 只触及 cost_total>0（已按旧公式落库）、有缓存命中且旧缓存项>0 的行；cost_total=0 的存量行
// 由读取侧按新公式回算，无需改动。
//
// 近似：单价是在某天中途补配的那一天，行内只有部分调用落了花费，此处按整行缓存
// 比例扣减（假设已计价部分的命中率与整行相同）。
func fixCostCacheDoubleCount(tx *gorm.DB) (string, error) {
	// 旧缓存项 > 0 才说明该行确实按「全价 + 缓存价」双计过；未配缓存单价的行
	// （旧缓存项 = 0）在新公式下命中部分同样按全价计，结果不变，不能扣减。
	const scope = `cost_total > 0 AND cost_input > 0 AND cache_read_tokens > 0 AND input_tokens > 0
		AND cost_total - cost_input - cost_output > 1e-12`
	// 登记说明用全表合计（修正后的行不再满足 scope，不能用 scope 求「修正后」）。
	sumAll := func() (float64, error) {
		var v float64
		err := tx.Raw(`SELECT COALESCE(SUM(cost_total), 0) FROM stats_usage_daily`).Scan(&v).Error
		return v, err
	}
	before, err := sumAll()
	if err != nil {
		return "", err
	}
	res := tx.Exec(`UPDATE stats_usage_daily SET
		cost_input = cost_input
			- cost_input * MIN(cache_read_tokens, input_tokens) * 1.0 / input_tokens
			+ MAX(cost_total - cost_input - cost_output, 0),
		cost_total = cost_total
			- cost_input * MIN(cache_read_tokens, input_tokens) * 1.0 / input_tokens
		WHERE ` + scope)
	if res.Error != nil {
		return "", res.Error
	}
	after, err := sumAll()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("rows=%d table_cost_total_before=%.4f table_cost_total_after=%.4f", res.RowsAffected, before, after), nil
}
