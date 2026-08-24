package buildinfo

import "testing"

func TestGet(t *testing.T) {
	info := Get()

	if info.GoVersion == "" {
		t.Fatal("GoVersion should be set")
	}
	if info.Version == "" {
		t.Fatal("Version should be non-empty")
	}
	// gitShort 不应超过 12 字符（hash 本身更短则保持原样）
	if len(info.GitShort) > 12 {
		t.Fatalf("gitShort too long: %q", info.GitShort)
	}
	// 多次调用应返回一致快照（sync.Once 缓存）
	if info2 := Get(); info2.GitRevision != info.GitRevision || info2.BuildTime != info.BuildTime {
		t.Fatalf("Get() returned inconsistent snapshots: %+v vs %+v", info, info2)
	}
}
