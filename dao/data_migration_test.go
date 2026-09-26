package dao

import (
	"math"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&UsageDaily{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// oldCost 复刻修复前的落库公式（input 含缓存，却又叠加缓存项）。
func oldCost(in, cr, out int, pin, pout, pc float64) (ci, co, ct float64) {
	ci = float64(in) * pin / 1e6
	co = float64(out) * pout / 1e6
	ct = ci + co + float64(cr)*pc/1e6
	return
}

func TestFixCostCacheDoubleCount(t *testing.T) {
	db := newMigrationTestDB(t)
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)

	// A：按旧公式落库、有缓存命中 → 需修正
	ciA, coA, ctA := oldCost(1_000_000, 800_000, 100_000, 8, 28, 2)
	// B：无缓存命中 → 旧新公式一致，不动
	ciB, coB, ctB := oldCost(1_000_000, 0, 100_000, 8, 28, 2)
	// C：未配缓存单价（旧缓存项=0）→ 新公式命中部分同样按全价，不动
	ciC, coC, ctC := oldCost(1_000_000, 500_000, 0, 4, 12, 0)
	rows := []UsageDaily{
		{BotID: "b", Model: "glm-5.3", Feature: "a", Date: day, InputTokens: 1_000_000, CacheReadTokens: 800_000, OutputTokens: 100_000, CostInput: ciA, CostOutput: coA, CostTotal: ctA},
		{BotID: "b", Model: "glm-5.3", Feature: "b", Date: day, InputTokens: 1_000_000, OutputTokens: 100_000, CostInput: ciB, CostOutput: coB, CostTotal: ctB},
		{BotID: "b", Model: "m-nocache", Feature: "c", Date: day, InputTokens: 1_000_000, CacheReadTokens: 500_000, CostInput: ciC, CostOutput: coC, CostTotal: ctC},
		// D：存量行（cost_total=0，读取侧回算）→ 不动
		{BotID: "b", Model: "glm-5.3", Feature: "d", Date: day, InputTokens: 1_000_000, CacheReadTokens: 900_000},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 2; i++ { // 第二次必须是空操作（恰好一次）
		if err := runDataMigrations(db); err != nil {
			t.Fatalf("run #%d: %v", i+1, err)
		}
	}

	get := func(feature string) UsageDaily {
		var r UsageDaily
		if err := db.Where("feature = ?", feature).First(&r).Error; err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := get("a")
	// 正确值：input = 0.2M*8 + 0.8M*2 = 3.2；output = 2.8；total = 6.0（旧 11.6）
	if !near(a.CostInput, 3.2) || !near(a.CostOutput, 2.8) || !near(a.CostTotal, 6.0) {
		t.Fatalf("A = (%v,%v,%v), want (3.2,2.8,6.0)", a.CostInput, a.CostOutput, a.CostTotal)
	}
	if b := get("b"); !near(b.CostTotal, ctB) || !near(b.CostInput, ciB) {
		t.Fatalf("B changed: %+v", b)
	}
	if c := get("c"); !near(c.CostTotal, ctC) || !near(c.CostInput, ciC) {
		t.Fatalf("C changed: %+v", c)
	}
	if d := get("d"); d.CostTotal != 0 || d.CostInput != 0 {
		t.Fatalf("D (legacy) changed: %+v", d)
	}

	var reg []DataMigration
	if err := db.Find(&reg).Error; err != nil {
		t.Fatal(err)
	}
	if len(reg) != 1 || reg[0].Name != costCacheDoubleCountFixName {
		t.Fatalf("registry = %+v", reg)
	}
}
