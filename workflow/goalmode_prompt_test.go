package workflow

import (
	"strings"
	"testing"
)

// 这些测试锁定「目标模式对 LLM 的可发现性」这一契约。
//
// 背景：task 工具是 DeferredLoad 的，未加载时 llm.ToolDeferral.View() 会把
// Parameters 置为 nil，模型此时只能看到 Name + Description。因此如果
// goalMode 只写在参数 schema 里，模型在决定是否使用它之前根本看不到该能力。
// 下面的断言确保这条链路上的每一环都不会在后续改动中被悄悄改掉。

func TestSubmitToolDescriptionMentionsGoalMode(t *testing.T) {
	def := submitToolDef(nil)

	// Description 是延迟加载未展开时模型唯一可见的信息。
	if !strings.Contains(def.Tool.Description, "goalMode") {
		t.Errorf("task 工具的 Description 必须提到 goalMode，否则延迟加载状态下模型无从得知该能力\nDescription: %s", def.Tool.Description)
	}

	// 参数 schema 里仍需保留完整定义。
	params, ok := def.Tool.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters 类型异常: %T", def.Tool.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters.properties 缺失")
	}
	gm, ok := props["goalMode"].(map[string]any)
	if !ok {
		t.Fatal("参数 schema 中缺少 goalMode")
	}
	if gm["type"] != "boolean" {
		t.Errorf("goalMode 类型应为 boolean，实际 %v", gm["type"])
	}
	if desc, _ := gm["description"].(string); !strings.Contains(desc, "闭环") {
		t.Errorf("goalMode 参数描述应说明闭环行为，实际: %s", desc)
	}
}

func TestSubmitToolKeywordsCoverGoalMode(t *testing.T) {
	def := submitToolDef(nil)

	// tool_search 只按 name / description / keywords 做子串匹配（见 llm.toolMatches）。
	// 用户说「目标模式」时，必须能搜到 task 工具。
	want := []string{"目标模式", "闭环"}
	for _, w := range want {
		found := false
		for _, k := range def.Tool.Keywords {
			if strings.Contains(k, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("task 工具的 Keywords 应包含 %q，否则 tool_search 搜不到；当前: %v", w, def.Tool.Keywords)
		}
	}
}

func TestWorkflowPromptSectionExplainsGoalMode(t *testing.T) {
	content := workflowToolPromptSection.Content

	if !workflowToolPromptSection.Enabled {
		t.Fatal("workflow 提示词段落必须启用")
	}

	// 必须有独立小节，而不是塞在开头长句里——LLM 对结构化决策规则的遵循度明显更高。
	if !strings.Contains(content, "## 目标模式") {
		t.Error("提示词应包含独立的「## 目标模式」小节")
	}
	// 必须同时给出正例与反例，否则模型容易无差别开启。
	for _, marker := range []string{"应当开启", "不要开启", "goalMode: true"} {
		if !strings.Contains(content, marker) {
			t.Errorf("提示词缺少关键说明: %q", marker)
		}
	}
}

func TestStatusToolDescribesGoalIteration(t *testing.T) {
	def := statusToolDef(nil)
	if !strings.Contains(def.Tool.Description, "goalIteration") {
		t.Errorf("task_status 描述应说明目标模式轮次字段，便于模型解读返回值\nDescription: %s", def.Tool.Description)
	}
}

func TestGoalModeAnalyzerHintRequestsVerificationNode(t *testing.T) {
	// 分析器的 system prompt 是静态的，模型无法判断本次是否开启目标模式，
	// 因此必须靠这段任务侧提示告知，并要求产出可闭环的验收节点。
	for _, marker := range []string{"验收节点", "review", "feedback"} {
		if !strings.Contains(goalModeAnalyzerHint, marker) {
			t.Errorf("目标模式分析提示缺少 %q", marker)
		}
	}
	if !strings.Contains(goalModeAnalyzerHint, "不会构成环") {
		t.Error("提示应明确回退边不构成环，避免模型因担心成环而拒填 feedback")
	}
}
