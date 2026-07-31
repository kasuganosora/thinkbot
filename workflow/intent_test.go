package workflow

import "testing"

func TestDetectGoalModeIntent(t *testing.T) {
	positive := []string{
		"现在 review 所有后端模块 修复所有发现的问题，直到没新问题被发现为止",
		"修复所有测试直到全部通过",
		"重构这个模块，确保 lint 和构建都没有报错为止",
		"把这篇文章反复打磨到可以直接发布的质量",
		"逐个审查每个模块，审查到没有新问题才进行下一个",
		"清理项目里所有 TypeScript 类型错误，直到没有为止",
		"多轮打磨这份文档",
		"一遍遍检查这个实现直到稳定",
		"Fix all failing tests, iterate until they all pass",
		"Keep refining the code until no new issues",
		"run the linter and fix until clean",
	}
	for _, tc := range positive {
		if !DetectGoalModeIntent(tc) {
			t.Errorf("expected GOAL intent, got false: %q", tc)
		}
	}

	negative := []string{
		"调研 Redis 和 Memcached 的差异并写一份对比",
		"把这三个文件翻译成英文",
		"统计一下代码库有多少个 Go 文件",
		"帮我看看这段代码的 bug",
		"写一个快速排序函数",
		"今天天气怎么样",
		"确保服务器配置正确", // 单独「确保」不应误触发
		"全部通过测试后再合并", // 无「直到/反复」等收敛动词
		"",
	}
	for _, tc := range negative {
		if DetectGoalModeIntent(tc) {
			t.Errorf("expected NON-goal intent, got true: %q", tc)
		}
	}
}

func TestGoalModeDirective(t *testing.T) {
	orig := "修复所有测试直到全部通过"
	d := GoalModeDirective(orig)
	if d == orig {
		t.Fatal("directive should wrap the original text")
	}
	if !goalContains(d, orig) {
		t.Errorf("directive must contain the original requirement; got %q", d)
	}
	if !goalContains(d, "goalMode: true") {
		t.Errorf("directive must mention goalMode: true; got %q", d)
	}
	if !goalContains(d, "禁止") {
		t.Errorf("directive must forbid subagent/delegate; got %q", d)
	}
}

func goalContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
