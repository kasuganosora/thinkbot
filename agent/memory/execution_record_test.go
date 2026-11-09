package memory

import (
	"strings"
	"testing"
)

func TestStripExecutionRecords(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "clean text unchanged",
			in:   "已经改好了，可以上线。",
			want: "已经改好了，可以上线。",
		},
		{
			name: "complete pair removed with content",
			in:   "开头 " + ExecutionRecordTagOpen + "Command: go build" + ExecutionRecordTagClose + " 结尾",
			want: "开头  结尾",
		},
		{
			name: "unclosed opening tag drops to end",
			in:   "开头 " + ExecutionRecordTagOpen + "Command: secret output",
			want: "开头",
		},
		{
			name: "two blocks",
			in:   ExecutionRecordTagOpen + "a" + ExecutionRecordTagClose + "x" + ExecutionRecordTagOpen + "b" + ExecutionRecordTagClose,
			want: "x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripExecutionRecords(tc.in); got != tc.want {
				t.Fatalf("StripExecutionRecords = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripExecutionRecords_Idempotent(t *testing.T) {
	in := "hi " + ExecutionRecordTagOpen + "x" + ExecutionRecordTagClose + " there"
	once := StripExecutionRecords(in)
	if twice := StripExecutionRecords(once); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
	if strings.Contains(once, "execution_record") {
		t.Fatalf("tag leaked: %q", once)
	}
}
