package workflow

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
)

func newSharedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.WorkflowModel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestGet_SeesWritesFromAnotherRepositoryInstance 是本次事故的回归测试。
//
// 同一进程里存在两个 Repository 实例：
//   - api/botservice.go 为每个 bot 建的引擎，真正执行工作流并写 DB
//   - api/workflow_service.go 建的引擎，专门服务 HTTP 查询
//
// 二者各有独立内存缓存。旧 Get 无条件信任缓存命中，于是 API 侧永远返回自己创建工作流
// 那一刻的快照（status=analyzing、nodeCount=0），而真实进度只在 bot 侧缓存与 DB 里，
// 表现为「后端在跑、UI 永远显示分析中」，刷新和清浏览器缓存都无效。
func TestGet_SeesWritesFromAnotherRepositoryInstance(t *testing.T) {
	db := newSharedDB(t)
	log := zap.NewNop().Sugar()

	writer := NewRepository(db, log) // 模拟 bot 侧：执行工作流
	reader := NewRepository(db, log) // 模拟 API 侧：服务 HTTP 查询

	wf := NewWorkflow("wf-1", "req", nil)
	wf.Status = WorkflowAnalyzing
	if err := writer.Save(wf); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	// reader 先读一次，把 analyzing 快照放进自己的缓存（对应 API 侧创建后立刻查询）。
	got, err := reader.Get("wf-1")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got.Status != WorkflowAnalyzing {
		t.Fatalf("Status = %v, want analyzing", got.Status)
	}

	// writer 推进工作流：拆出节点并转入 running。
	time.Sleep(5 * time.Millisecond) // 保证 updated_at 严格变新
	wf.Status = WorkflowRunning
	wf.Nodes = []*DAGNode{
		{ID: "n1", Name: "a", Status: NodeCompleted},
		{ID: "n2", Name: "b", Status: NodeRunning},
	}
	if err := writer.Save(wf); err != nil {
		t.Fatalf("progress save: %v", err)
	}

	// reader 再读：必须看到新状态，而不是自己缓存里的 analyzing。
	got, err = reader.Get("wf-1")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got.Status != WorkflowRunning {
		t.Errorf("Status = %v, want running; the reader is serving a stale cached snapshot", got.Status)
	}
	if len(got.Nodes) != 2 {
		t.Errorf("node count = %d, want 2; stale cache would report 0", len(got.Nodes))
	}
}

// TestGet_UsesCacheWhenDBUnchanged 验证 DB 未变化时仍走缓存。
//
// 新鲜度校验不能退化成「每次都反序列化整个 Data」，否则 1.5s 轮询会持续放大 DB 读。
func TestGet_UsesCacheWhenDBUnchanged(t *testing.T) {
	db := newSharedDB(t)
	repo := NewRepository(db, zap.NewNop().Sugar())

	wf := NewWorkflow("wf-2", "req", nil)
	wf.Status = WorkflowRunning
	if err := repo.Save(wf); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 直接篡改 DB 里的 Data 但**不动** updated_at：若 Get 仍返回旧状态，
	// 说明它确实用了缓存而没有无条件重新反序列化。
	if err := db.Model(&dao.WorkflowModel{}).
		Where("id = ?", "wf-2").
		UpdateColumn("data", `{"id":"wf-2","status":"failed"}`).Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}

	got, err := repo.Get("wf-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != WorkflowRunning {
		t.Errorf("Status = %v, want running (cache should be used when updated_at is unchanged)", got.Status)
	}
}

// TestSave_KeepsModelAndDomainTimestampsInSync 验证写库后缓存时间戳被 DB 真实值校准。
//
// WorkflowModel.UpdatedAt 命中 gorm 的 autoUpdateTime，保存时会被 gorm 覆盖成自己的
// time.Now()，比领域对象上的值略晚。若不把它同步回缓存，写入者自己的缓存会在每次读时
// 被判过期，凭空多一次 DB 反序列化。
func TestSave_KeepsModelAndDomainTimestampsInSync(t *testing.T) {
	db := newSharedDB(t)
	repo := NewRepository(db, zap.NewNop().Sugar())

	wf := NewWorkflow("wf-3", "req", nil)
	if err := repo.Save(wf); err != nil {
		t.Fatalf("save: %v", err)
	}

	var model dao.WorkflowModel
	if err := db.First(&model, "id = ?", "wf-3").Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if model.UpdatedAt.After(wf.UpdatedAt) {
		t.Errorf("DB updated_at (%v) is newer than the in-memory snapshot (%v); "+
			"the writer's own cache would be judged stale on every read",
			model.UpdatedAt, wf.UpdatedAt)
	}

	// 连续读取不应因时间戳漂移而反复重载：状态必须稳定。
	for i := 0; i < 3; i++ {
		got, err := repo.Get("wf-3")
		if err != nil {
			t.Fatalf("get #%d: %v", i, err)
		}
		if got.ID != "wf-3" {
			t.Fatalf("unexpected workflow: %+v", got)
		}
	}
}
