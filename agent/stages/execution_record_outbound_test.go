package stages

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

const execRecordLeak = "<execution_record>\nRuntime execution record (system-generated).\nCommand: go build ./... (exit 0)\n</execution_record>"

// The execution record is injected as a system message in the LLM context. If
// the model echoes it, all three outbound paths must strip it with the same
// rule (channel text / stored public block / Web stream).
func TestExecutionRecord_OutboundParity(t *testing.T) {
	raw := "已经查完了。\n" + execRecordLeak + "\n可以上线。"
	want := CleanOutboundText(raw)
	if strings.Contains(want, "execution_record") || strings.Contains(want, "go build ./...") {
		t.Fatalf("channel text leaked execution record: %q", want)
	}
	if !strings.Contains(want, "已经查完了。") || !strings.Contains(want, "可以上线。") {
		t.Fatalf("normal reply content lost: %q", want)
	}
	if got := cleanPublicBlock(raw); strings.Contains(got, "execution_record") || strings.Contains(got, "go build ./...") {
		t.Fatalf("public block leaked execution record: %q", got)
	}
	for _, replyTags := range []bool{false, true} {
		f := NewOutputCleanStreamFilter(replyTags)
		var got strings.Builder
		for i := 0; i < len(raw); i++ {
			got.WriteString(f.Feed(raw[i : i+1]))
		}
		got.WriteString(f.Flush())
		if out := got.String(); out != want {
			t.Fatalf("replyTags=%v: stream %q != outbound %q", replyTags, out, want)
		}
	}
}

// A block inside <public> must not leak while the surrounding public text is
// still delivered in real time.
func TestOutputCleanStreamFilter_DropsExecutionRecordInPublic(t *testing.T) {
	for _, replyTags := range []bool{false, true} {
		f := NewOutputCleanStreamFilter(replyTags)
		chunks := []string{
			"<public>可见开头 ",
			memory.ExecutionRecordTagOpen,
			"Command: git push origin main (exit 0)",
			memory.ExecutionRecordTagClose,
			" 可见结尾</public>",
		}
		var got string
		for _, c := range chunks {
			got += f.Feed(c)
		}
		got += f.Flush()
		if strings.Contains(got, "execution_record") || strings.Contains(got, "git push origin") {
			t.Fatalf("replyTags=%v: stream leaked execution record: %q", replyTags, got)
		}
		if !strings.Contains(got, "可见开头") {
			t.Fatalf("replyTags=%v: public text lost: %q", replyTags, got)
		}
	}
}

func TestOutputCleanStreamFilter_UnclosedExecutionRecord(t *testing.T) {
	raw := "开头 " + execRecordLeak[:len(execRecordLeak)-len(memory.ExecutionRecordTagClose)]
	for _, replyTags := range []bool{false, true} {
		f := NewOutputCleanStreamFilter(replyTags)
		var got strings.Builder
		for i := 0; i < len(raw); i++ {
			got.WriteString(f.Feed(raw[i : i+1]))
		}
		got.WriteString(f.Flush())
		if out := got.String(); strings.Contains(out, "go build ./...") || strings.Contains(out, "execution_record") {
			t.Fatalf("replyTags=%v: unclosed block leaked: %q", replyTags, out)
		}
	}
}
