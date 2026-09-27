package memory

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestDetailIdentifiers(t *testing.T) {
	text := "用户绑定授权码TB-RW5Q-SKZN，参考 https://github.com/easyeda/easyeda-api-skill/blob/main/README.md 。" +
		"改了 files.go 和 memory.md，提交 a75c993，订单 A202608041785811765，端口 8080，年份 26，版本 v2。"
	got := strings.Join(detailIdentifiers(text), " | ")
	for _, want := range []string{"TB-RW5Q-SKZN", "https://github.com/easyeda/easyeda-api-skill/blob/main/README.md", "files.go", "memory.md", "a75c993", "A202608041785811765", "8080"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, not := range []string{"| 26", "v2"} {
		if strings.Contains(got, not) {
			t.Errorf("%q should not be an identifier: %s", not, got)
		}
	}
	if ids := detailIdentifiers("用户喜欢猫，经常在晚上画画。"); len(ids) != 0 {
		t.Errorf("plain text has no identifiers: %v", ids)
	}
}

type scriptedProvider struct {
	mu        sync.Mutex
	responses []string
	calls     []llm.GenerateParams
}

func (s *scriptedProvider) Name() string { return "scripted" }
func (s *scriptedProvider) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	text := "[]"
	if len(s.responses) > 0 {
		text, s.responses = s.responses[0], s.responses[1:]
	}
	return &llm.GenerateResult{Text: text, FinishReason: llm.FinishReasonStop}, nil
}
func (s *scriptedProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

var guardInputs = []ClusterInput{
	{ID: "m1", Content: "用户绑定了授权码 TB-RW5Q-SKZN"},
	{ID: "m2", Content: "用户是 ThinkBot 管理员，常分享技能仓库 https://github.com/infometa/workbuddyskills"},
	{ID: "m3", Content: "用户喜欢猫"},
	{ID: "m4", Content: "用户养了一只猫"},
}

func TestClusterMerge_LossyClusterRepaired(t *testing.T) {
	p := &scriptedProvider{responses: []string{
		`[{"merged_content":"用户是ThinkBot管理员","source_ids":["m1","m2"]},{"merged_content":"用户喜欢猫并养了一只","source_ids":["m3","m4"]}]`,
		`[{"merged_content":"用户是ThinkBot管理员，授权码 TB-RW5Q-SKZN，常分享 https://github.com/infometa/workbuddyskills","source_ids":["m1","m2"]}]`,
	}}
	got, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", guardInputs, 128000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 2 {
		t.Fatalf("want 1 repair call, got %d calls", len(p.calls))
	}
	repair := p.calls[1].Messages
	last := ""
	if len(repair) == 3 {
		last = repair[2].Content[0].(llm.TextPart).Text
	}
	if !strings.Contains(last, "TB-RW5Q-SKZN") || !strings.Contains(last, "m1, m2") {
		t.Fatalf("repair request should list the missing identifiers: %+v", repair)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 clusters, got %+v", got)
	}
	for _, cl := range got {
		if sourceKey(cl.SourceIDs) == sourceKey([]string{"m1", "m2"}) && !strings.Contains(cl.MergedContent, "TB-RW5Q-SKZN") {
			t.Fatalf("lossy version accepted: %+v", cl)
		}
	}
}

func TestClusterMerge_LossyClusterDroppedWhenRepairFails(t *testing.T) {
	p := &scriptedProvider{responses: []string{
		`[{"merged_content":"用户是ThinkBot管理员","source_ids":["m1","m2"]},{"merged_content":"用户喜欢猫并养了一只","source_ids":["m3","m4"]}]`,
		`[{"merged_content":"用户是ThinkBot管理员，有授权码","source_ids":["m1","m2"]}]`,
	}}
	got, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", guardInputs, 128000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || sourceKey(got[0].SourceIDs) != sourceKey([]string{"m3", "m4"}) {
		t.Fatalf("only the lossless cluster may survive: %+v", got)
	}
}

func TestClusterMerge_NoRepairWhenLossless(t *testing.T) {
	p := &scriptedProvider{responses: []string{
		`[{"merged_content":"用户喜欢猫并养了一只","source_ids":["m3","m4","ghost"]}]`,
	}}
	got, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", guardInputs, 128000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 1 || len(got) != 1 {
		t.Fatalf("calls=%d clusters=%+v", len(p.calls), got)
	}
	if len(got[0].SourceIDs) != 2 {
		t.Fatalf("unknown source id must be removed: %v", got[0].SourceIDs)
	}
}
