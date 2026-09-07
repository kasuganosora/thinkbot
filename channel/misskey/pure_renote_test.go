package misskey

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
)

// TestHandleNotePureRenoteMetadata 覆盖纯 Renote 的 is_pure_renote 标记：
// 纯 Renote（Renote 指向的帖子、本身无正文）必须被 handleNote 打上
// core.MetaIsPureRenote=true，供后续 pure-renote enricher 硬抑制回复，
// 避免对物理不可回复的纯 Renote 发起注定失败的 notes/create 请求（CANNOT_REPLY_TO_A_PURE_RENOTE）。
//
// 构造说明：纯 Renote 仍会把被 Renote 帖的正文作为对话内容（renoteFallback），
// 因此这里用「带正文的被 Renote 帖」让 handleNote 走到构建 metadata 的分支（不会因 text 为空而提前 return）。
func TestHandleNotePureRenoteMetadata(t *testing.T) {
	cases := []struct {
		name       string
		note       Note
		wantPure   bool
		shouldDrop bool // handleNote 因 text 为空且无附件提前 return，不会注入 ingress
	}{
		{
			name: "纯 Renote：自身无正文、Renote 目标有正文",
			note: Note{
				ID:      "note-pure-renote-1",
				User:    User{ID: "user-1", Username: "alice"},
				RenoteID: "note-orig-1",
				Renote:  &Note{ID: "note-orig-1", Text: "被转发的原始帖子内容", User: User{ID: "bob"}},
			},
			wantPure: true,
		},
		{
			name: "普通原创帖：无 Renote，不应标记",
			note: Note{
				ID:   "note-orig-2",
				User: User{ID: "user-2", Username: "carol"},
				Text: "今天天气不错",
			},
			wantPure: false,
		},
		{
			name: "引用转发（带评论）：自身有正文，不应标记",
			note: Note{
				ID:      "note-quote-1",
				User:    User{ID: "user-3", Username: "dave"},
				Text:    "我也这么觉得",
				RenoteID: "note-orig-3",
				Renote:  &Note{ID: "note-orig-3", Text: "被引用的帖子", User: User{ID: "eve"}},
			},
			wantPure: false,
		},
		{
			name: "纯 Renote 且被 Renote 帖也无正文：handleNote 提前 return",
			note: Note{
				ID:      "note-pure-empty-1",
				User:    User{ID: "user-4", Username: "frank"},
				RenoteID: "note-orig-4",
				Renote:  &Note{ID: "note-orig-4", User: User{ID: "grace"}},
			},
			wantPure:   true, // 逻辑上仍属纯 Renote
			shouldDrop: true, // 但 text 与 renote 正文都为空，handleNote 提前 return，不注入 ingress
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ing := inbound.NewIngress(inbound.IngressConfig{BufferSize: 8}, zap.NewNop().Sugar(), noop.NewTracerProvider())
			defer ing.Close()

			c := &MisskeyChannel{ingress: ing, mentionRe: testMentionRe("kanna")}
			// eventType=mention, mentioned=true 以走完整投递路径
			c.handleNote(context.Background(), tc.note, "mention", true)

			if tc.shouldDrop {
				// 提前 return，ingress 不应收到任何消息
				select {
				case env := <-ing.C():
					t.Fatalf("纯空 Renote 不应注入 ingress，但收到了: %+v", env.Message)
				case <-time.After(50 * time.Millisecond):
				}
				return
			}

			select {
			case env := <-ing.C():
				got, ok := env.Message.Metadata[core.MetaIsPureRenote].(bool)
				if !ok {
					t.Fatalf("metadata 缺少 %q 键", core.MetaIsPureRenote)
				}
				if got != tc.wantPure {
					t.Errorf("metadata[%q] = %v, want %v (noteID=%s)", core.MetaIsPureRenote, got, tc.wantPure, tc.note.ID)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("handleNote 未在超时内注入 ingress（noteID=%s）", tc.note.ID)
			}
		})
	}
}
