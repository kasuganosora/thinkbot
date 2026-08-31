package sandbox

import "testing"

// TestLastNonEmptyLine 钉住 docker CLI 输出的「结果行」解析。
//
// 背景（真实故障场景）：容器内以非 root 降权运行时，若 HOME 指向不可写目录，
// docker CLI 每次调用都会往 stderr 打一行
//
//	WARNING: Error loading config file: /root/.docker/config.json: permission denied
//
// 早前 snapshot() 把 stdout 与 stderr 合并进同一 buffer 后整体 TrimSpace 当作
// 镜像 ID 解析，该警告会被当成 ID 前缀，`strings.Index(id, ":")` 切到
// "Error loading config file" 里，最终产出垃圾快照 ID "Error loading co"。
// 现已改为分流 stderr + 取最后一行，本测试防止回退。
func TestLastNonEmptyLine(t *testing.T) {
	const sha = "sha256:5a34a168e86b58b0dba3c446f2b405537501dd1c389683a1526a67ba0ab4e40c"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"单行", sha, sha},
		{"尾部换行", sha + "\n", sha},
		{"尾部多个空行", sha + "\n\n  \n", sha},
		{
			// 即便警告意外混入 stdout，也不再污染结果。
			name: "前置警告行",
			in:   "WARNING: Error loading config file: /root/.docker/config.json: permission denied\n" + sha + "\n",
			want: sha,
		},
		{"空输入", "", ""},
		{"仅空白", "  \n\t\n", ""},
	}

	for _, tt := range tests {
		if got := lastNonEmptyLine(tt.in); got != tt.want {
			t.Errorf("%s: lastNonEmptyLine() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestSnapshotIDParsing 复现 snapshot() 的 ID 截取逻辑，确认在有警告干扰时
// 仍得到正确的 12 位短 ID（而非 "Error loading co"）。
func TestSnapshotIDParsing(t *testing.T) {
	parse := func(stdout string) string {
		id := lastNonEmptyLine(stdout)
		if i := indexColon(id); i >= 0 {
			id = id[i+1:]
		}
		if len(id) > 12 {
			id = id[:12]
		}
		return id
	}

	const want = "5a34a168e86b"

	if got := parse("sha256:5a34a168e86b58b0dba3c446f2b405537501dd1c389683a1526a67ba0ab4e40c\n"); got != want {
		t.Errorf("干净输出: got %q, want %q", got, want)
	}

	polluted := "WARNING: Error loading config file: /root/.docker/config.json: permission denied\n" +
		"sha256:5a34a168e86b58b0dba3c446f2b405537501dd1c389683a1526a67ba0ab4e40c\n"
	if got := parse(polluted); got != want {
		t.Errorf("带警告输出: got %q, want %q（回退为整体解析会得到 \"Error loading co\"）", got, want)
	}
}

func indexColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
