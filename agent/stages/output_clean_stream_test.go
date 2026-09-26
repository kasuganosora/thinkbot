package stages

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// streamClean 按 processStream 的组合方式（reply-control 过滤 → 标签清洗）处理切片。
func streamClean(chunks []string, replyTags bool) (string, []string) {
	var rc ReplyControlStreamFilter
	oc := NewOutputCleanStreamFilter(replyTags)
	var outs []string
	pub := func(s string) {
		if s != "" {
			outs = append(outs, s)
		}
	}
	for _, c := range chunks {
		pub(oc.Feed(rc.Feed(c)))
	}
	pub(oc.Feed(rc.Flush()))
	pub(oc.Flush())
	return strings.Join(outs, ""), outs
}

// sharedClean 是其他渠道 send:true 时的出站内容（与 LLMStage.Process 门控同一条链）。
func sharedClean(raw string) string {
	text := CleanOutboundText(raw)
	if _, clean, ok := parseReplyControl(text); ok {
		text = clean
	}
	return extractPublicReply(text)
}

// runeSplits 返回在 rune 边界上的全部二分切法（真实供应商按完整字符下发增量）。
func runeSplits(s string) [][]string {
	var out [][]string
	for i := range s {
		if i == 0 {
			continue
		}
		out = append(out, []string{s[:i], s[i:]})
	}
	return append(out, []string{s})
}

func perRune(s string) []string {
	var out []string
	for len(s) > 0 {
		_, n := utf8.DecodeRuneInString(s)
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

func randomChunks(s string, r *rand.Rand) []string {
	var out []string
	runes := perRune(s)
	for len(runes) > 0 {
		n := 1 + r.Intn(6)
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, strings.Join(runes[:n], ""))
		runes = runes[n:]
	}
	return out
}

// allChunkings 覆盖：整段、所有二分切点、逐字符、若干随机切法。
func allChunkings(s string) [][]string {
	out := runeSplits(s)
	out = append(out, perRune(s))
	r := rand.New(rand.NewSource(int64(len(s))))
	for i := 0; i < 30; i++ {
		out = append(out, randomChunks(s, r))
	}
	return out
}

// parityCases：流式清洗后的拼接结果必须与其他渠道最终发送的内容逐字相同。
var parityCases = []struct {
	name string
	raw  string
}{
	{"internal then public + control", "<internal>用户在打招呼，回一句</internal>\n<public>你好呀～今天想聊什么？</public>\n@@REPLY_CONTROL@@{\"send\": true}"},
	{"public only multi paragraph", "<public>第一段\n\n第二段：\n```go\nfunc a() {\n    return\n}\n```</public>\n@@REPLY_CONTROL@@{\"send\": true}"},
	{"plain text + control", "部署测试收到，一切正常 ✅\n\n@@REPLY_CONTROL@@{\"send\": true}"},
	{"nested internal", "<internal>a<internal>b</internal>c</internal><public>ok</public>"},
	{"nested public", "<public>a<public>b</public>c</public>"},
	{"internal inside public", "<public>hi <internal>secret</internal> there</public>"},
	{"unclosed internal inside public", "<public>hi <internal>secret</public> tail"},
	{"unclosed internal after public", "<public>hi</public><internal>never closed"},
	{"internal only unclosed", "<internal>never closed and more text"},
	{"internal only closed", "<internal>只有心里话</internal>"},
	{"text after public dropped", "<public>hi</public> trailing <b>text</b>"},
	{"empty public then real", "<public>  </public><public>real one</public><public>dup</public>"},
	{"think then public", "<think>plan step 1\nstep 2</think>\n<public>答</public>"},
	{"think inside public", "<public>a<think>x</public>y</think>b</public>"},
	{"unclosed think", "ok<think>never closed"},
	{"mixed case tags", "<INTERNAL>x</Internal><Public>Hi</PUBLIC>"},
	{"stray html plain", "Hello <b>world</b>!\n<p>para</p>"},
	{"malformed public close", "答案在这/public>"},
	{"malformed internal", "<public>x/internal> y</public>"},
	{"lt not a tag", "1 < 2 and 3<4, a<b"},
	{"url slashes", "see https://example.com/path/index.html and a/b/c"},
	{"leading and trailing whitespace", "  \n <public>\n  spaced  \n</public>\n  "},
	{"attributes tag", "<public>see <a href=\"x\">link</a></public>"},
}

func TestOutputCleanStream_ParityWithSharedCleaning(t *testing.T) {
	for _, c := range parityCases {
		want := sharedClean(c.raw)
		for _, chunks := range allChunkings(c.raw) {
			got, _ := streamClean(chunks, true)
			if got != want {
				t.Fatalf("%s: chunks=%q\n got %q\nwant %q", c.name, chunks, got, want)
			}
		}
	}
}

// TestOutputCleanStream_InternalNeverStreamed：无论怎么切，<internal>/<think> 里的内容、
// 标签文本本身都不会出现在任何一段推送的 delta 里（包括闭合标签在后续 chunk 才到的情况）。
func TestOutputCleanStream_InternalNeverStreamed(t *testing.T) {
	raws := []string{
		"<internal>SECRET1</internal><public>ok</public>",
		"前言 <internal>SECRET1 跨\n多行</internal> 后记",
		"<public>a<internal>SECRET1</internal>b</public>",
		"<internal>SECRET1 <public>SECRET2</public> still</internal><public>ok</public>",
		"<think>SECRET1</think>ok<internal>SECRET2",
		"<internal>SECRET1</INTERNAL>tail",
	}
	for _, raw := range raws {
		for _, chunks := range allChunkings(raw) {
			got, outs := streamClean(chunks, true)
			if strings.Contains(got, "SECRET") {
				t.Fatalf("secret leaked: raw=%q chunks=%q got=%q", raw, chunks, got)
			}
			for _, o := range outs {
				if strings.ContainsAny(o, "<>") {
					t.Fatalf("tag text leaked in delta %q (raw=%q chunks=%q)", o, raw, chunks)
				}
			}
		}
	}
}

// TestOutputCleanStream_HoldBackOnlyPartialTag：普通文本立即放行，只扣住可能的半截标签。
func TestOutputCleanStream_HoldBackOnlyPartialTag(t *testing.T) {
	f := NewOutputCleanStreamFilter(true)
	if got := f.Feed("你好，这是正文"); got != "你好，这是正文" {
		t.Fatalf("plain text should pass through immediately, got %q", got)
	}
	if got := f.Feed("继续<inter"); got != "继续" {
		t.Fatalf("partial tag should be held back, got %q", got)
	}
	if got := f.Feed("nal>秘密"); got != "" {
		t.Fatalf("internal content must be suppressed, got %q", got)
	}
	if got := f.Feed("还是秘密</inte"); got != "" {
		t.Fatalf("still inside internal, got %q", got)
	}
	if got := f.Feed("rnal>"); got != "" {
		t.Fatalf("closing tag must be dropped, got %q", got)
	}
	// 出现过 <internal> 后，区块外裸文本不再放行（最终出站也不会包含它）。
	if got := f.Feed("外面的字<pub"); got != "" {
		t.Fatalf("outside text after internal should be suppressed, got %q", got)
	}
	if got := f.Feed("lic>公开"); got != "公开" {
		t.Fatalf("public content should stream, got %q", got)
	}
	if got := f.Feed("内容 "); got != "内容" {
		t.Fatalf("trailing whitespace held back, got %q", got)
	}
	if got := f.Feed("</public>之后"); got != "" {
		t.Fatalf("content after first public block must be dropped, got %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("flush got %q", got)
	}

	// 非标签的 '<' 立即放行；半截 "<b" 在流尾原样放行（门控同样保留）。
	g := NewOutputCleanStreamFilter(true)
	if got := g.Feed("1 < 2"); got != "1 < 2" {
		t.Fatalf("got %q", got)
	}
	if got := g.Feed(" a<b"); got != " a" {
		t.Fatalf("got %q", got)
	}
	if got := g.Flush(); got != "<b" {
		t.Fatalf("flush got %q", got)
	}

	// 超过 maxStreamTagLen 仍无 '>'：不再扣住。
	h := NewOutputCleanStreamFilter(true)
	long := "<x" + strings.Repeat("y", maxStreamTagLen)
	if got := h.Feed(long); got != long {
		t.Fatalf("over-long pseudo tag should be released, got %q", got)
	}
}

// TestOutputCleanStream_ReplyTagsDisabled：门控未开启（其他渠道不剥 public/internal）时，
// 流式也只剥 think 块，其余标签原样保留，与出站内容一致。
func TestOutputCleanStream_ReplyTagsDisabled(t *testing.T) {
	raws := []string{
		"<think>plan</think>Hello <b>world</b> <public>x</public>",
		"a<thinking>x</thinking>b<internal>y</internal>",
		"ok<think>unclosed",
		"1 < 2 <thin",
	}
	for _, raw := range raws {
		want := CleanOutboundText(raw)
		for _, chunks := range allChunkings(raw) {
			got, _ := streamClean(chunks, false)
			if got != want {
				t.Fatalf("raw=%q chunks=%q got %q want %q", raw, chunks, got, want)
			}
		}
	}
}

// TestOutputCleanStream_KnownLimitation：<internal> 之前、没有 <public> 的裸文本在流式时
// 无法预知后面会不会出现 <internal>（门控会因此整段 fail-closed），所以已推送；
// 但 internal 内容本身绝不推送，落库/回放以门控 payload 为准。
func TestOutputCleanStream_KnownLimitation(t *testing.T) {
	got, _ := streamClean([]string{"前言 ", "<internal>x</internal>", " 后记"}, true)
	if got != "前言" {
		t.Fatalf("got %q", got)
	}
	if sharedClean("前言 <internal>x</internal> 后记") != "" {
		t.Fatal("shared cleaning should fail closed here")
	}
}

// TestProcess_WebStreamMatchesReplyPayload 端到端：开启回复控制门控的 Web 私聊，
// 推给 EventBus 的增量拼接结果 == 门控产出的 ActionReply payload（即其他渠道会发送的内容），
// 且 send:false 的私聊 fail-open 语义不变。
func TestProcess_WebStreamMatchesReplyPayload(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "internal + public + control split everywhere",
			chunks: []string{"<inte", "rnal>用户问", "候，简短回应</in", "ternal>\n<pu", "blic>晚上好", "！要不要", "聊聊今天？</pub", "lic>\n@@REPLY", "_CONTROL@@{\"se", "nd\": true}"},
			want:   "晚上好！要不要聊聊今天？",
		},
		{
			name:   "send false in private chat still replies with public text",
			chunks: []string{"<internal>其实不太想回</internal>", "<public>好的，", "收到。</public>", "\n@@REPLY_CONTROL@@", "{\"send\": false}"},
			want:   "好的，收到。",
		},
		{
			name:   "missing control falls back to explicit public",
			chunks: []string{"<internal>想法</internal><public>", "只发这句", "</public>"},
			want:   "只发这句",
		},
		{
			name:   "multi paragraph markdown keeps formatting",
			chunks: []string{"<public>第一段\n", "\n第二段\n```go\n", "    x := 1\n```", "</public>\n@@REPLY_CONTROL@@{\"send\": true}"},
			want:   "第一段\n\n第二段\n```go\n    x := 1\n```",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &recordingPublisher{}
			stage := NewLLMStage("llm", &chunkStreamProvider{chunks: c.chunks}, LLMConfig{
				RequireReplyControl: true,
				StreamPublisher:     pub,
				MessageBuilder: func(msg core.Message) []llm.Message {
					return []llm.Message{llm.UserMessage(msg.Text)}
				},
			}, nil, nil)
			env := core.NewEnvelope(core.Message{
				ID: "web-1", TraceID: "t-web-1", BotID: "b1", Text: "hi", Source: "web",
				Channel: "web", UserID: "1", ChatType: core.ChatPrivate,
			})
			out, err := stage.Process(context.Background(), env)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			replies := replyActions(out)
			if len(replies) != 1 {
				t.Fatalf("want 1 reply action, got %d", len(replies))
			}
			payload, _ := replies[0].Payload.(string)
			streamed := strings.Join(pub.deltas, "")
			if payload != c.want {
				t.Fatalf("payload = %q, want %q", payload, c.want)
			}
			if streamed != payload {
				t.Fatalf("streamed %q != payload %q (deltas=%q)", streamed, payload, pub.deltas)
			}
		})
	}
}
