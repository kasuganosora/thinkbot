package subagent

import "testing"

func TestWithReasoningEffort(t *testing.T) {
	sa := New(nil, "glm-5.3", WithReasoningEffort("low"))
	p := sa.buildParams(nil)
	if p.ReasoningEffort == nil || *p.ReasoningEffort != "low" {
		t.Fatalf("effort not sent: %v", p.ReasoningEffort)
	}
	if p := New(nil, "glm-5.3").buildParams(nil); p.ReasoningEffort != nil {
		t.Fatalf("default must not send reasoning_effort: %v", *p.ReasoningEffort)
	}
	if p := New(nil, "m", WithReasoningEffort("low"), WithReasoningEffort("")).buildParams(nil); p.ReasoningEffort != nil {
		t.Fatal(`"" must clear`)
	}
}
