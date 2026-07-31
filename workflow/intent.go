package workflow

import "strings"

// DetectGoalModeIntent 判断用户需求是否表达了「需反复打磨 / 审查直到达到质量验收标准」的
// 收敛性意图。这类请求应启用目标模式（闭环迭代），而不是一次性产出或内联处理。
//
// 仅匹配高精度短语，避免误伤普通请求：
//   - 显式「直到 X 为止 / 通过 / 没有…」的验收式表述
//   - 「反复打磨 / 迭代到 / review 到没有 / 一遍遍 / 多轮打磨」等收敛动词
//   - 英文 iterate until / keep fixing until / no new issues / converge 等
//
// 命中则返回 true，调用方应据此强制使用 task(goalMode: true)。
func DetectGoalModeIntent(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)

	// 1) 高精度强短语（直接命中）
	strong := []string{
		"反复打磨", "反复修改", "反复修", "反复审查", "反复 review", "反复检查", "反复调",
		"迭代到", "迭代直到", "迭代到通过",
		"review 到没有", "review到没有", "审查到没有", "审查到没", "review 到没",
		"没有新问题", "没新问题", "没有新错误", "没新错误", "no new issue", "no new issues",
		"打磨到", "修到没有", "改到没有", "修到没", "改到没",
		"一遍遍", "一遍一遍",
		"多轮打磨", "多轮迭代", "多轮审查",
		"做完了才算", "做完才算数", "做完才算", "完成才算", "通过才算",
		"直到达标", "直到合格", "直到满意", "直到干净", "直到 clean", "直到达标为止",
		"iterate until", "keep fixing until", "refine until", "polish until", "loop until",
		"no new errors", "converge", "until no", "until all pass", "until it passes",
		"pass all tests", "review until", "fix until", "反复 review",
	}
	for _, p := range strong {
		if strings.Contains(s, p) || strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}

	// 2) 「直到…[终止条件]」复合：出现「直到」且全文含合格/收敛词即命中
	//    （终止词不必紧接「直到」，如「直到全部通过」「直到没有为止」）
	if strings.Contains(s, "直到") {
		terminal := []string{
			"为止", "通过", "没有", "无", "报错", "错误", "问题", "干净", "满意",
			"合格", "达标", "不再", "稳定", "修复", "解决", "clean", "pass", "no ", "fixed", "新",
		}
		for _, term := range terminal {
			if strings.Contains(s, term) || strings.Contains(lower, strings.ToLower(term)) {
				return true
			}
		}
	}
	// 2b) 「…为止」复合：出现「为止」且含合格/收敛词（如「没有报错为止」「全部通过为止」）
	if strings.Contains(s, "为止") {
		terminal2 := []string{
			"没有", "无", "通过", "报错", "错误", "干净", "满意", "合格", "达标",
			"解决", "修复", "clean", "pass", "fixed", "新",
		}
		for _, term := range terminal2 {
			if strings.Contains(s, term) || strings.Contains(lower, strings.ToLower(term)) {
				return true
			}
		}
	}

	// 3) 「反复」+ 打磨/修/改/审/查/验/调 等收敛动词
	if strings.Contains(s, "反复") {
		for _, v := range []string{"打磨", "修", "改", "审", "查", "review", "验", "调", "处理"} {
			if strings.Contains(s, "反复"+v) {
				return true
			}
		}
	}

	return false
}

// GoalModeDirective 为目标模式意图请求生成一条强制路由指令，追加在用户原始需求之前。
// 该指令要求模型必须使用 task(goalMode: true) 提交，禁止用 subagent/delegate 内联处理。
// 调用方应将原始需求原文保留（用于持久化），仅把本指令 + 原文作为注入模型的文本。
func GoalModeDirective(original string) string {
	const head = `[目标模式指令 / GOAL MODE DIRECTIVE]
本任务有「做到达标才算完成、需反复打磨或审查直到没有新问题」的明确验收要求。
你【必须】使用 task 工具提交，并显式传入 goalMode: true，把下面的原始需求作为 requirement。
【禁止】用 subagent / delegate / 一次性内联处理来完成，也【不要】一轮跑完就结束——
目标模式会在审查不通过时自动回退重做，直到通过或达到最大轮数（默认 5 轮）。
原始需求如下：`
	return head + "\n\n" + original
}
