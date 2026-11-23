package route

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestNarrowToolsDropsHeavyTools(t *testing.T) {
	in := []llm.Tool{
		{Name: "memory"},
		{Name: "now"},
		{Name: "sandbox_exec"},
		{Name: "workflow_task"},
		{Name: "calculate"},
	}
	got := NarrowTools(in)
	names := map[string]bool{}
	for _, tool := range got {
		names[tool.Name] = true
	}
	if !names["memory"] || !names["now"] || !names["calculate"] {
		t.Fatalf("kept = %#v", names)
	}
	if names["sandbox_exec"] || names["workflow_task"] {
		t.Fatalf("heavy tools leaked: %#v", names)
	}
}

// A task/followup must never run on the light tier, even when L1 explicitly
// asks for it: the light tool surface has no exec/file tools and would invite
// a fabricated completion report.
func TestNormalizeTier_TaskCannotUseLight(t *testing.T) {
	for _, intent := range []string{IntentTask, IntentFollowup} {
		gotIntent, gotModel := normalizeTier(intent, ModelLight)
		if gotIntent != intent || gotModel != ModelDefault {
			t.Fatalf("normalizeTier(%q, light) = (%q, %q)", intent, gotIntent, gotModel)
		}
		env := core.NewEnvelope(core.Message{Text: "继续改"})
		SetTier(env, intent, ModelLight)
		if WantsLight(env) {
			t.Fatalf("SetTier(%q, light) must not produce a light turn", intent)
		}
	}
	for _, intent := range []string{IntentChat, IntentProgress} {
		_, gotModel := normalizeTier(intent, ModelLight)
		if gotModel != ModelLight {
			t.Fatalf("chat/progress must still be allowed on the light tier: %q", gotModel)
		}
	}
}

// A turn with inline media must never be demoted to the text-only light tier:
// the message builder injects media parts based on the main model, so a light
// execution would send them to a model that rejects non-text content parts.
func TestWantsLight_InlineMediaForcesFullTier(t *testing.T) {
	cases := []struct {
		name string
		atts []core.Attachment
		want bool // want light tier
	}{
		{"no attachments", nil, true},
		{"plain file", []core.Attachment{{Type: core.AttachmentTypeFile, MimeType: "text/plain", Data: []byte("hi")}}, true},
		{"image without uri", []core.Attachment{{Type: core.AttachmentTypeImage, MimeType: "image/png"}}, true},
		{"image with inline data", []core.Attachment{{Type: core.AttachmentTypeImage, MimeType: "image/png", Data: []byte("png")}}, false},
		{"image with url", []core.Attachment{{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/a.png"}}, false},
		{"audio", []core.Attachment{{Type: core.AttachmentTypeAudio, MimeType: "audio/mp3", Data: []byte("mp3")}}, false},
		{"video", []core.Attachment{{Type: core.AttachmentTypeVideo, MimeType: "video/mp4", Data: []byte("mp4")}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := core.NewEnvelope(core.Message{Text: "这是什么？"})
			if tc.atts != nil {
				core.SetAttachments(&env.Message, tc.atts)
			}
			SetTier(env, IntentChat, ModelLight)
			if got := WantsLight(env); got != tc.want {
				t.Fatalf("WantsLight = %v, want %v", got, tc.want)
			}
			if got := HasInlineMedia(env); got != !tc.want {
				t.Fatalf("HasInlineMedia = %v, want %v", got, !tc.want)
			}
			// The tier decision itself is unchanged; only the execution is forced
			// to the full model. Callers can still log why the demotion was skipped.
			if ModelOf(env) != ModelLight {
				t.Fatal("tier stamp must stay as chosen by L1")
			}
		})
	}
}

// Metadata arriving from a channel is often a []any of maps rather than
// []core.Attachment; the guard must work for both representations.
func TestWantsLight_InlineMediaFromNormalizedMetadata(t *testing.T) {
	env := core.NewEnvelope(core.Message{Text: "看图"})
	env.Message.Metadata = map[string]any{
		"attachments": []any{
			map[string]any{"type": "image", "mimeType": "image/png", "url": "data:image/png;base64,AAAA"},
		},
	}
	SetTier(env, IntentChat, ModelLight)
	if WantsLight(env) {
		t.Fatal("normalized image attachment must keep the turn on the full tier")
	}
	if HasInlineMedia(nil) {
		t.Fatal("nil envelope must not report media")
	}
}

func TestUpgradeToolRequestsFullTierOnce(t *testing.T) {
	env := core.NewEnvelope(core.Message{Text: "run the tests"})
	SetTier(env, IntentChat, ModelLight)
	tool := NewUpgradeTool(env)
	_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, nil)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !UpgradeRequested(env) {
		t.Fatal("expected upgrade request")
	}
	MarkUpgraded(env)
	if WantsLight(env) {
		t.Fatal("upgraded turn must leave the light tier")
	}
}
