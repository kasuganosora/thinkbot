package stages

import (
	"strings"
	"testing"
)

// 本文件钉死「</public> 之后的裸文本」这条放宽路径的闸门（2026-10-12 复查 3987099）。
//
// 3987099 把标签外文本并入公开回复，是为了修「模型只把过场话包进 <public>、真正答复
// 写在标签外，答复被整段丢掉」的事故；代价是这条路径从 fail-closed 变成了 fail-open。
// 因此闸门必须：
//  1. 正常答复绝不能被拦（否则事故回归，用户收不到回复）；
//  2. 中文内心独白 / 私有标签残片 / 协议标记必须被拦（否则心里话发到频道）。
//
// 注意：既有检测 only 覆盖英文（nakedPlanningRe 要求 "but/however + I"、"I should ... Step N"），
// 中文 bot 一句都命中不了，所以这里每一条中文用例都是这次补上的。

func TestTrailingGate_KeepsRealAnswer(t *testing.T) {
	// 事故样本：过场话在 <public> 里，真正答复写在标签外 —— 必须照发。
	raw := "<public>容栞娜翻翻资料</public>查完啦，这个是 Re0 里老吴的梗，出处是 2018 年的一段直播。"
	text, trailingLen, adopted := extractPublicReplyDetailed(CleanOutboundText(raw))
	if !adopted {
		t.Fatalf("real answer after </public> must be adopted, got text=%q trailingLen=%d", text, trailingLen)
	}
	if !strings.Contains(text, "查完啦") || !strings.Contains(text, "容栞娜翻翻资料") {
		t.Fatalf("both blocks must survive: %q", text)
	}
}

func TestTrailingGate_BlocksChineseSoliloquy(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"心里话元话语", "<public>好的呀</public>心里想：这个人又在乱来了，我其实很不想管，但不能说出口。"},
		{"自言自语标记", "<public>收到</public>（自言自语）算了，先不想这个，我暗自决定下次不回他了。"},
		{"不该说出去", "<public>嗯嗯</public>他这个方案其实漏洞很大，不过我不应该告诉他，免得伤人。"},
		{"私有标签残片", "<public>好的</public>这部分是我的判断 <internal 没闭合，不该出去"},
		{"其它协议标记", "<public>收到</public>@@TOOL_RESULT@@ {\"ok\": true}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, trailingLen, adopted := extractPublicReplyDetailed(CleanOutboundText(tc.raw))
			if adopted {
				t.Fatalf("private-looking trailing must be dropped, got adopted text=%q", text)
			}
			if trailingLen == 0 {
				t.Fatal("trailingLen must be reported even when dropped (audit signal)")
			}
			if !strings.Contains(text, "<public>") && text == "" {
				t.Fatalf("dropping the trailing must not also drop the public block: %q", text)
			}
			// 公开区块本身必须保住
			if !strings.Contains(text, "好的") && !strings.Contains(text, "收到") && !strings.Contains(text, "嗯") {
				t.Fatalf("public block lost: %q", text)
			}
		})
	}
}

func TestTrailingGate_NoTrailingReportedWhenAbsent(t *testing.T) {
	raw := "<public>只有一段回复</public>"
	text, trailingLen, adopted := extractPublicReplyDetailed(CleanOutboundText(raw))
	if trailingLen != 0 || adopted {
		t.Fatalf("no trailing must report 0/false, got len=%d adopted=%v", trailingLen, adopted)
	}
	if text != "只有一段回复" {
		t.Fatalf("text = %q", text)
	}
}

func TestLooksLikePrivateTrailing_CoversEnglishSignals(t *testing.T) {
	// 英文既有信号（nakedPlanningRe）必须继续生效，不能被这次的改动替换掉。
	if !looksLikePrivateTrailing("But I failed to check. Options: (a) retry (b) give up") {
		t.Fatal("english naked planning must still be detected")
	}
}

func TestLooksLikePrivateTrailing_DoesNotEatNormalReplies(t *testing.T) {
	// 闸门误判的代价是「答复被丢回 3987099 之前的事故状态」，必须确认正常中文回复放行。
	normal := []string{
		"我先查一下再告诉大小姐。",
		"这个我需要分三步说明：第一步装依赖，第二步跑测试，第三步看日志。",
		"其实我觉得可以这样：先确认授权，再选低谷时段。",
		"好的呀，栞娜记进小本本了（",
	}
	for _, s := range normal {
		if looksLikePrivateTrailing(s) {
			t.Fatalf("normal reply must not be treated as private: %q", s)
		}
	}
}
