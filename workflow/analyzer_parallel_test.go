package workflow

import (
	"strings"
	"testing"
)

// TestAnalyzerPrompt_PushesForParallelism 守住分析器提示词里的并行指引。
//
// 事故（2026-08-03）：一次「逐模块审查前端」的需求被拆成 9 个节点，但依赖被串成一条链
// （n1 → n2 → ... → n9），每个模块都等上一个做完。这些审查任务彼此独立，本应并行，
// 串行让总耗时成倍增加，还让任一节点失败就级联 skip 掉后面全部节点。
//
// 原提示词只有一句「无依赖的节点将并行执行」和一个抽象的 A/B/C/D 例子，不足以纠正
// 模型「按顺序做事」的倾向。这里断言强化后的关键要素仍在。
func TestAnalyzerPrompt_PushesForParallelism(t *testing.T) {
	required := []struct {
		fragment string
		why      string
	}{
		{"Maximize Parallelism", "需要一个显式的并行小节，而不是把规则埋在依赖说明里"},
		{"Default to running sub-tasks in parallel", "必须给出默认立场：先假设无依赖，有真实数据依赖才加"},
		{"must read the earlier task's output", "需要可操作的判定口诀，让模型自问是否存在数据依赖"},
		{"The most common mistake", "正例/反例对照比单纯描述有效得多"},
		{"single chain", "必须点名最常见的误判形态"},
	}
	for _, r := range required {
		if !strings.Contains(analyzerSystemPrompt, r.fragment) {
			t.Errorf("analyzerSystemPrompt missing %q: %s", r.fragment, r.why)
		}
	}
}

// TestAnalyzerPrompt_DecompositionDefaultsToParallel 验证「分解原则」本身就主张并行。
//
// 原文是中性的「识别哪些串行、哪些并行」，模型容易偏向串行；改为明确的默认并行立场。
func TestAnalyzerPrompt_DecompositionDefaultsToParallel(t *testing.T) {
	if !strings.Contains(analyzerSystemPrompt, "Default to running sub-tasks in parallel") {
		t.Error("分解原则应明确主张默认并行，否则模型倾向于按顺序串联子任务")
	}
}
