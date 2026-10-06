package stages

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

const (
	incidentFiller = "容栞娜翻翻资料，这个梗栞娜不敢乱答"
	incidentAnswer = "这个栞娜真不敢乱答……\n\n486、蕾姆老婆、青色断章这些栞娜都知道，但“老吴”栞娜没查到确切的出处，不敢瞎编一个糊弄大小姐（\n\n是夺还编里的哪个角色吗？大小姐揭晓一下，栞娜记进小本本（"
)

// TestExtractPublicReply_IncidentSecondBlockDelivered 回归 2026-10-06 21:49 TG 私聊：
// 第 1 步的过场话 + 最后一步的真正答复是两个不同的 <public> 区块，真正答复不能丢。
func TestExtractPublicReply_IncidentSecondBlockDelivered(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["incident1006"])
	want := incidentFiller + "\n\n" + incidentAnswer
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestExtractPublicReply_MultiBlockRules(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"0926 near-identical (one-char typo) sent once", histMultiBlockReplies["dup0926Typo"],
			"ランプ集めの積み重ねが一瞬で消えるのは本当にしんどいですよね……あれはもう青春のログみたいなものだから、引き継ぎミスのダメージがデカい。ランプ消えたのいつ頃でした？"},
		{"0927 status + correction both kept", histMultiBlockReplies["distinct0927Correction"],
			"抱歉喵，刚才光顾着说没真发出去……这次真的发了！\n\n呜……又没发出去，这次是权限被拦了。大小姐在 Misskey 那边直接 @栞娜 说一句「把 tokyo-transit 发条帖」之类的，栞娜收到就能发了喵"},
		{"5fb36fa half/full-width duplicate sent once",
			"<public>喵~6/6全对!💯 这是给谁复习呀🤔</public><public>喵~6/6全对！💯 这是给谁复习呀🤔</public>",
			"喵~6/6全对!💯 这是给谁复习呀🤔"},
		{"short punctuation variant sent once", "<public>好的！</public><public>好的～</public>", "好的！"},
		{"identical emoji-only sent once", "<public>😂</public><public>😂</public>", "😂"},
		{"different emoji-only both kept", "<public>😂</public><public>👍</public>", "😂\n\n👍"},
		{"three blocks: dup in the middle dropped", "<public>先查一下</public><public>先查一下。</public><public>查到了：答案是 42</public>",
			"先查一下\n\n查到了：答案是 42"},
		{"empty and internal-only blocks skipped", "<public> </public><public><internal>只有心里话</internal></public><public>真正的回复</public>", "真正的回复"},
		{"public nested in top-level internal is private", "<internal>想想 <public>SECRET</public> 算了</internal><public>ok</public>", "ok"},
		{"unclosed trailing public not taken", "<public>完整的一句</public><public>被截断的半", "完整的一句"},
		{"all empty blocks", "<public></public><internal>静默观察，不打扰</internal><public></public>\n@@REPLY_CONTROL@@{\"send\": false}", ""},
		// 0925 线上形态：<internal> 被错写的 </public> "闭合"，真正的 </internal> 在后面。
		{"stray close inside internal", "<internal>想法</public><internal>\n更多想法\n</internal><public>@someone 真的是这样（苦笑）</public>\n\n@@REPLY_CONTROL@@{\"send\": true}",
			"@someone 真的是这样（苦笑）"},
	}
	for _, c := range cases {
		if got := sharedClean(c.raw); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// TestExtractPublicReply_RewriteDeduped：0929 Web 回复把同一段话改写了一遍（前缀 0.83 / 全文 0.58），
// 只发第一版。
func TestExtractPublicReply_RewriteDeduped(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["rewrite0929"])
	if !strings.HasPrefix(got, "收到收到，嘴已焊死") || strings.Contains(got, "库你就留着吧") {
		t.Fatalf("rewrite should be deduped to the first version, got %q", got)
	}
}

// TestExtractPublicReply_GarbledTurnKeepsFinalAnswer：0927 B 站登录那轮前面几个区块是崩坏输出，
// 最后一步才是真正的坦白与说明；只取第一块时这段会丢。
func TestExtractPublicReply_GarbledTurnKeepsFinalAnswer(t *testing.T) {
	got := sharedClean(histMultiBlockReplies["garbled0927"])
	if !strings.HasSuffix(got, "扫完说一声，栞娜马上验证真实状态（这次只报验证过的事实，不再抢跑喵）。") {
		t.Fatalf("final step's public block must be delivered, got %q", got)
	}
	if strings.Contains(got, "上一个回复完全崩溃了") {
		t.Fatalf("internal leaked: %q", got)
	}
}

func TestPublicBlockSimilarityThresholds(t *testing.T) {
	var set publicBlockSet
	set.add(incidentFiller)
	if set.isDuplicate(incidentAnswer) {
		t.Fatal("filler and real answer must not be duplicates")
	}
	if !set.isDuplicate("容栞娜翻翻资料,这个梗栞娜不敢乱答!") {
		t.Fatal("punctuation/width variant must be a duplicate")
	}
	// prefixVerdict 不能与 isDuplicate 矛盾：提前判定 distinct 的必须确实不是重复。
	long := strings.Repeat("完全不同的另一段内容", 6)
	if decided, distinct := set.prefixVerdict(long); !decided || !distinct || set.isDuplicate(long) {
		t.Fatalf("prefixVerdict(long) = %v,%v", decided, distinct)
	}
	var short publicBlockSet
	short.add("第一段公开。")
	if short.isDuplicate("第二段公开。") {
		t.Fatal("short blocks differing in a meaningful char must not be duplicates")
	}
	if !short.isDuplicate("第一段公开！") {
		t.Fatal("short punctuation variant must be a duplicate")
	}
	if decided, _ := set.prefixVerdict("短"); decided {
		t.Fatal("short block must not be decided early")
	}
}

// TestOutputCleanStream_MultiBlockParity：真实多区块样本在任意切法下，流式拼接结果与
// 渠道发送 / 落库内容（extractPublicReply）逐字相同。
func TestOutputCleanStream_MultiBlockParity(t *testing.T) {
	raws := map[string]string{
		"near dup short":           "<public>好的！</public><internal>x</internal><public>好的～</public>",
		"three blocks":             "<public>先查一下</public><public>先查一下。</public><public>查到了：答案是 42</public>",
		"emoji":                    "<public>😂</public>\n<public>👍</public>",
		"long distinct":            "<public>稍等</public><public>" + strings.Repeat("这是第二段真正的回答内容，", 8) + "</public>",
		"long prefix dup":          "<public>" + strings.Repeat("一模一样的开头文字", 8) + "结尾A</public><public>" + strings.Repeat("一模一样的开头文字", 8) + "结尾B</public>",
		"prefix same tail differs": "<public>" + strings.Repeat("同样的开头", 12) + "</public><public>" + strings.Repeat("同样的开头", 12) + strings.Repeat("后面完全不一样的大段补充说明", 10) + "</public>",
		"nested in internal":       "<internal>想想 <public>SECRET</public> 算了</internal><public>ok</public>",
		"unclosed second short":    "<public>完整的一句</public><public>被截断的半",
	}
	for k, v := range histMultiBlockReplies {
		// garbled* 是模型崩坏输出，<public> 之前有大段裸文本（流式已知限制：见
		// TestOutputCleanStream_KnownLimitation），且 strayTagRe 会把 "/data/..." 吞到下一个 '>'，
		// 两者与多区块无关、改动前就不一致；只在下面单独断言最终答复不丢。
		if strings.HasPrefix(k, "garbled") {
			continue
		}
		raws[k] = v
	}
	for name, raw := range raws {
		want := sharedClean(raw)
		for _, chunks := range allChunkings(raw) {
			got, outs := streamClean(chunks, true)
			if got != want {
				t.Fatalf("%s: chunks=%q\n got %q\nwant %q", name, chunks, got, want)
			}
			for _, o := range outs {
				if strings.Contains(o, "SECRET") {
					t.Fatalf("%s: secret leaked in %q", name, o)
				}
			}
		}
	}
}

// TestOutputCleanStream_SecondBlockStreamsLive：确定不是重复后，第二个区块不等闭合就实时推送。
func TestOutputCleanStream_SecondBlockStreamsLive(t *testing.T) {
	f := NewOutputCleanStreamFilter(true)
	if got := f.Feed("<public>" + incidentFiller + "</public>"); got != incidentFiller {
		t.Fatalf("first block: %q", got)
	}
	if got := f.Feed("<public>这个"); got != "" {
		t.Fatalf("second block must be held until it is known not to be a duplicate, got %q", got)
	}
	if got := f.Feed("栞娜真不敢乱答……486、蕾姆老婆、青色断章"); got != "" {
		t.Fatalf("still too short to decide, got %q", got)
	}
	rest := "这些栞娜都知道，但老吴栞娜没查到确切的出处，不敢瞎编一个糊弄大小姐（"
	if got := f.Feed(rest); got != "\n\n这个栞娜真不敢乱答……486、蕾姆老婆、青色断章"+rest {
		t.Fatalf("distinct block should be released once decided, got %q", got)
	}
	if got := f.Feed("是夺还编里的哪个角色吗？"); got != "是夺还编里的哪个角色吗？" {
		t.Fatalf("then streams live, got %q", got)
	}
	if got := f.Feed("</public>\n@@REPLY"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("flush got %q", got)
	}
}

// incidentChunks 是 21:49 事故回复按增量切开的样子（过场话 → 工具 → 最终答复，result.Text 跨步骤拼接）。
func incidentChunks() []string {
	return []string{
		"<public>容栞娜翻翻资料，", "这个梗栞娜不敢乱答</public>",
		"<public>这个栞娜真不敢乱答……\n\n486、蕾姆老婆、青色断章这些栞娜都知道，",
		"但“老吴”栞娜没查到确切的出处，不敢瞎编一个糊弄大小姐（\n\n",
		"是夺还编里的哪个角色吗？大小姐揭晓一下，栞娜记进小本本（</public>",
		"\n\n@@REPLY_CONTROL@@{\"send\": true}",
	}
}

// TestProcess_MultiBlockReplyPayload 端到端：Telegram 私聊（渠道发送 + chat_messages 落库都用
// ActionReply payload）与 Web 私聊（流式 + payload）对多区块回复给出同一内容。
func TestProcess_MultiBlockReplyPayload(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"incident: filler + real answer", incidentChunks(), incidentFiller + "\n\n" + incidentAnswer},
		{"near-identical blocks sent once",
			[]string{"<public>喵~6/6全对!💯 这是给谁复习呀🤔</public>", "<public>喵~6/6全对！💯 这是给谁复习呀🤔</public>", "\n@@REPLY_CONTROL@@{\"send\": true}"},
			"喵~6/6全对!💯 这是给谁复习呀🤔"},
		{"missing control: explicit public fallback keeps both blocks",
			[]string{"<public>先查一下</public>", "<internal>查到了</internal><public>答案是 42</public>"},
			"先查一下\n\n答案是 42"},
	}
	for _, c := range cases {
		for _, source := range []string{"telegram", "web"} {
			t.Run(c.name+"/"+source, func(t *testing.T) {
				pub := &recordingPublisher{}
				cfg := LLMConfig{
					RequireReplyControl: true,
					MessageBuilder: func(msg core.Message) []llm.Message {
						return []llm.Message{llm.UserMessage(msg.Text)}
					},
				}
				if source == "web" {
					cfg.StreamPublisher = pub
				}
				stage := NewLLMStage("llm", &chunkStreamProvider{chunks: c.chunks}, cfg, nil, nil)
				env := core.NewEnvelope(core.Message{
					ID: "m-1", TraceID: "t-1", BotID: "b1", Text: "老吴是什么梗", Source: source,
					Channel: source, UserID: "76017910", ChatType: core.ChatPrivate,
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
				if payload != c.want {
					t.Fatalf("payload = %q, want %q", payload, c.want)
				}
				if source == "web" {
					if streamed := strings.Join(pub.deltas, ""); streamed != payload {
						t.Fatalf("streamed %q != payload %q", streamed, payload)
					}
				}
			})
		}
	}
}
