package interaction

import (
	"encoding/json"
	"testing"
)

func TestSSEPayloadRoundTrip(t *testing.T) {
	// Simulate the exact UserChoiceEventPayload shape (inline to avoid import cycle)
	type ucOption struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}
	type ucPayload struct {
		Type       string     `json:"type"`
		QuestionID string     `json:"questionId"`
		Question   string     `json:"question"`
		Options    []ucOption `json:"options"`
		Mode       string     `json:"mode"`
		InputHint  string     `json:"inputHint,omitempty"`
		Timeout    int        `json:"timeout"`
		TimeoutAt  int64      `json:"timeoutAt"`
		Via        string     `json:"via"`
	}

	ev := ucPayload{
		Type: "user_choice", QuestionID: "uc-test123",
		Question: "测试题目：夜间同方向近距离跟车行驶，应使用什么灯光？",
		Options: []ucOption{
			{ID: "o0", Label: "A. 远光灯", Description: "远光照射前车"},
			{ID: "o1", Label: "B. 近光灯", Description: "近光照明"},
			{ID: "o2", Label: "C. 示廓灯", Description: "示宽示意"},
			{ID: "o3", Label: "D. 危险报警闪光灯"},
		},
		Mode: "single", InputHint: "或输入你的答案",
		Timeout: 600, TimeoutAt: 1788339000000, Via: "web",
	}

	// Simulate translateEventToSSE: wrap in map[string]any then JSON marshal
	data := map[string]any{
		"toolCallId":   "tc-001",
		"tool":         "user_choice",
		"invocationId": "inv-001",
		"stream":       "stdout",
		"chunk":        "",
		"payload":      ev,
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SSE data:\n%s", string(b))

	// Parse back and verify options survive
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	payload, _ := parsed["payload"].(map[string]any)
	if payload == nil {
		t.Fatal("payload is nil after round-trip!")
	}
	opts, _ := payload["options"].([]any)
	if len(opts) != 4 {
		t.Fatalf("expected 4 options, got %d", len(opts))
	}
	for i, o := range opts {
		m, _ := o.(map[string]any)
		t.Logf("  option[%d]: id=%v label=%v", i, m["id"], m["label"])
		if m["id"] == nil {
			t.Errorf("option[%d] has nil id - ChoiceCard would filter it out!", i)
		}
	}
}
