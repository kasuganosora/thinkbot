package engagement

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

type testClock struct {
	t time.Time
}

func newTestBreaker() (*OutreachBreaker, *testClock) {
	clk := &testClock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	b := NewOutreachBreaker(OutreachBreakerConfig{
		SilenceWindow:   3 * time.Minute,
		EpisodeBoundary: 24 * time.Hour,
	}, zap.NewNop().Sugar())
	b.nowFn = func() time.Time { return clk.t }
	return b, clk
}

func talkPast(userID string) *core.Message {
	return &core.Message{
		Channel:   "room-1",
		UserID:    userID,
		Text:      "anyway what about lunch",
		Mentioned: false,
		ChatType:  core.ChatGroup,
	}
}

func mention(userID string) *core.Message {
	return &core.Message{
		Channel:   "room-1",
		UserID:    userID,
		Text:      "hey bot",
		Mentioned: true,
		ChatType:  core.ChatGroup,
	}
}

func reactionFrom(channel, userID string) *core.Message {
	return &core.Message{
		Channel:   channel,
		UserID:    userID,
		Text:      "",
		Mentioned: false,
		ChatType:  core.ChatGroup, // Telegram 群表态：不是 @ 也不是私聊
		Metadata: map[string]any{
			core.MetaEventType:  core.MetaEventTypeReaction,
			core.MetaAckOnly:    true,
			core.MetaReactorIDs: []string{userID},
		},
	}
}

func TestOutreachBreaker_OneTalkPastDeclinesImmediately(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u1"))

	if !b.IsDeclined("room-1", "u1") {
		t.Fatal("one talk-past must decline immediately")
	}
	if b.IsPending("room-1", "u1") {
		t.Fatal("declined must clear pending")
	}
	suppress, reason := b.ShouldSuppress("room-1", "u1")
	if !suppress || reason != core.KVSuppressReasonUnanswered {
		t.Fatalf("should suppress, got suppress=%v reason=%q", suppress, reason)
	}
	if !b.ShouldStripReplyTarget("room-1", "u1") {
		t.Fatal("declined must strip reply_target")
	}
	if s, _ := b.ShouldSuppress("room-1", "u2"); s {
		t.Fatal("other user must not be suppressed")
	}
}

func TestOutreachBreaker_TalkPastDoesNotAffectOtherUser(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u2")) // 别人在说话，不算 u1 talk-past

	if !b.IsPending("room-1", "u1") {
		t.Fatal("u1 should still be pending")
	}
	if b.IsDeclined("room-1", "u1") {
		t.Fatal("u1 must not be declined from someone else's talk")
	}
}

func TestOutreachBreaker_NoSecondBidWhileAwaiting(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")

	suppress, reason := b.ShouldSuppress("room-1", "u1")
	if !suppress || reason != core.KVSuppressReasonUnanswered {
		t.Fatalf("awaiting must block a second bid, got suppress=%v reason=%q", suppress, reason)
	}
	if b.ShouldStripReplyTarget("room-1", "u1") {
		t.Fatal("pending is not declined; do not strip until declined")
	}
}

func TestOutreachBreaker_MentionResetsDeclined(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u1"))

	if s, _ := b.ShouldSuppress("room-1", "u1"); !s {
		t.Fatal("expected suppress before mention")
	}

	b.OnInbound(mention("u1"))
	if b.IsDeclined("room-1", "u1") || b.IsPending("room-1", "u1") {
		t.Fatal("@ must return to open")
	}
	if s, reason := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatalf("mention must lift suppress, reason=%q", reason)
	}
	if b.ShouldStripReplyTarget("room-1", "u1") {
		t.Fatal("@ must stop stripping reply_target")
	}
}

func TestOutreachBreaker_PrivateMessageCountsAsResponse(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("dm-u1", "u1")
	b.OnInbound(&core.Message{
		Channel:   "dm-u1",
		UserID:    "u1",
		Text:      "hi",
		Mentioned: false,
		ChatType:  core.ChatPrivate,
	})
	if b.IsDeclined("dm-u1", "u1") {
		t.Fatal("private inbound must reset, not count as unanswered")
	}
	if s, _ := b.ShouldSuppress("dm-u1", "u1"); s {
		t.Fatal("private response must not suppress")
	}
}

func TestOutreachBreaker_SilenceWindowLazySettle(t *testing.T) {
	b, clk := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")

	clk.t = clk.t.Add(2 * time.Minute)
	b.OnInbound(talkPast("u2")) // 窗口未到，u1 仍 pending
	if !b.IsPending("room-1", "u1") {
		t.Fatal("silence window not elapsed yet")
	}
	if b.IsDeclined("room-1", "u1") {
		t.Fatal("must not decline before silence window")
	}

	clk.t = clk.t.Add(2 * time.Minute) // 总共 4m > 3m
	b.OnInbound(talkPast("u2"))
	if !b.IsDeclined("room-1", "u1") {
		t.Fatal("silence should decline, no follow-up send")
	}
	if b.IsPending("room-1", "u1") {
		t.Fatal("silence settle must clear pending")
	}
}

func TestOutreachBreaker_LateResponseDoesNotCountSilence(t *testing.T) {
	b, clk := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	clk.t = clk.t.Add(10 * time.Minute)
	b.OnInbound(mention("u1")) // 先 reset，再 settle → 不应记拒绝
	if b.IsDeclined("room-1", "u1") {
		t.Fatal("late @ should reset, not count silence")
	}
}

func TestOutreachBreaker_TimePassingDoesNotLiftDeclined(t *testing.T) {
	b, clk := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u1"))

	clk.t = clk.t.Add(30 * time.Minute)
	if s, _ := b.ShouldSuppress("room-1", "u1"); !s {
		t.Fatal("30m must not lift declined — that would be a second bid")
	}
	clk.t = clk.t.Add(7 * time.Hour)
	if s, _ := b.ShouldSuppress("room-1", "u1"); !s {
		t.Fatal("7h must not lift declined")
	}
	if !b.IsDeclined("room-1", "u1") {
		t.Fatal("declined flag must persist")
	}
}

func TestOutreachBreaker_EpisodeBoundaryAllowsRoomLevelOnly(t *testing.T) {
	b, clk := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u1"))

	if s, _ := b.ShouldSuppress("room-1", "u1"); !s {
		t.Fatal("expected suppress within episode")
	}

	clk.t = clk.t.Add(24*time.Hour + time.Minute)
	if s, _ := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatal("after episode boundary, room-level engage is allowed")
	}
	if !b.ShouldStripReplyTarget("room-1", "u1") {
		t.Fatal("after episode boundary still must not pinpoint the user")
	}
	if !b.IsDeclined("room-1", "u1") {
		t.Fatal("declined stays until they initiate")
	}

	// 房间级发言不得记成对该人的新出价
	b.RecordProactiveReply("room-1", "u1")
	if b.IsPending("room-1", "u1") {
		t.Fatal("room-level send must not re-arm pending against a declined user")
	}
	b.OnInbound(talkPast("u1"))
	if s, _ := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatal("talk-past of a room comment must not restart the 24h suppress")
	}
}

func TestOutreachBreaker_MentionAfterEpisodeFullyOpens(t *testing.T) {
	b, clk := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")
	b.OnInbound(talkPast("u1"))
	clk.t = clk.t.Add(25 * time.Hour)

	b.OnInbound(mention("u1"))
	if b.IsDeclined("room-1", "u1") {
		t.Fatal("@ after episode must fully open")
	}
	if b.ShouldStripReplyTarget("room-1", "u1") {
		t.Fatal("@ must allow pinpoint again")
	}
	if s, _ := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatal("open must not suppress")
	}
}

func TestNotifyProactiveSent_OnlyProactiveReply(t *testing.T) {
	b, _ := newTestBreaker()

	NotifyProactiveSent(b, core.Action{
		Type:   core.ActionReply,
		UserID: "u1",
		Metadata: map[string]any{
			core.MetaEngagementProactive: true,
			core.MetaEngagementChannel:   "room-1",
		},
	})
	if !b.IsPending("room-1", "u1") {
		t.Fatal("proactive send should set pending")
	}
	if b.IsDeclined("room-1", "u1") {
		t.Fatal("record should only set pending, not declined")
	}
	b.OnInbound(talkPast("u1"))
	if !b.IsDeclined("room-1", "u1") {
		t.Fatal("proactive send should have been recorded")
	}

	b2, _ := newTestBreaker()
	NotifyProactiveSent(b2, core.Action{
		Type:   core.ActionReply,
		UserID: "u1",
		Metadata: map[string]any{
			core.MetaEngagementChannel: "room-1",
		},
	})
	b2.OnInbound(talkPast("u1"))
	if b2.IsDeclined("room-1", "u1") {
		t.Fatal("non-proactive reply must not be recorded")
	}
}

func TestOutreachBreaker_ReactionCountsAsUptakeNotTalkPast(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("room-1", "u1")

	b.OnInbound(reactionFrom("room-1", "u1"))
	if b.IsPending("room-1", "u1") || b.IsDeclined("room-1", "u1") {
		t.Fatal("like/reaction must open, not talk-past into declined")
	}
	if s, _ := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatal("reaction must lift awaiting")
	}
}

func TestOutreachBreaker_ReactionResetsMisskeyTimelineKey(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("misskey:timeline", "u1")
	b.OnInbound(&core.Message{
		Channel:   "misskey:timeline",
		UserID:    "u1",
		Text:      "whatever",
		Mentioned: false,
		ChatType:  core.ChatGroup,
	})
	if !b.IsDeclined("misskey:timeline", "u1") {
		t.Fatal("setup: declined on timeline")
	}

	// Misskey 反应入站 Channel 是 userID，不是 misskey:timeline
	b.OnInbound(reactionFrom("u1", "u1"))
	if b.IsDeclined("misskey:timeline", "u1") {
		t.Fatal("reaction must reset declined even when Channel ≠ timeline key")
	}
	if s, _ := b.ShouldSuppress("misskey:timeline", "u1"); s {
		t.Fatal("timeline suppress must lift after like")
	}
}

func TestOutreachBreaker_GroupedReactionResetsAllReactors(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("misskey:timeline", "u1")
	b.RecordProactiveReply("misskey:timeline", "u2")

	b.OnInbound(&core.Message{
		Channel:   "u1",
		UserID:    "u1",
		Mentioned: false,
		ChatType:  core.ChatPrivate,
		Metadata: map[string]any{
			core.MetaEventType:  core.MetaEventTypeReaction,
			core.MetaAckOnly:    true,
			core.MetaReactorIDs: []string{"u1", "u2"},
		},
	})
	if b.IsPending("misskey:timeline", "u1") || b.IsPending("misskey:timeline", "u2") {
		t.Fatal("grouped reaction must reset every reactor")
	}
}

func TestOutreachBreaker_IgnoresEmptyIDs(t *testing.T) {
	b, _ := newTestBreaker()
	b.RecordProactiveReply("", "u1")
	b.RecordProactiveReply("room-1", "")
	if s, _ := b.ShouldSuppress("room-1", "u1"); s {
		t.Fatal("empty ids must be no-ops")
	}
}

func TestDefaultOutreachBreakerConfig(t *testing.T) {
	cfg := DefaultOutreachBreakerConfig()
	if cfg.SilenceWindow != 3*time.Minute {
		t.Errorf("SilenceWindow=%v", cfg.SilenceWindow)
	}
	if cfg.EpisodeBoundary != 24*time.Hour {
		t.Errorf("EpisodeBoundary=%v", cfg.EpisodeBoundary)
	}
}
