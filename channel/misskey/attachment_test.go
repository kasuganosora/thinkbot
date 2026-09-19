package misskey

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestNoteAttachmentsNormalization 锁定 Misskey 入站文件归一化为 core.Attachment 的行为：
// 图片/音频/视频须带上正确的 Type（供 messageBuilder 直送或 MultimodalStage 转写），
// 其余归为普通文件；URL 透传 DriveFile 公开直链，下游 DataURI 优先返回 URL。
func TestNoteAttachmentsNormalization(t *testing.T) {
	files := []File{
		{Type: "image/png", URL: "https://misskey.example.com/files/abc.png", Name: "screenshot.png"},
		{Type: "audio/mpeg", URL: "https://misskey.example.com/files/def.mp3", Name: "voice.mp3"},
		{Type: "video/mp4", URL: "https://misskey.example.com/files/ghi.mp4", Name: "clip.mp4"},
		{Type: "application/pdf", URL: "https://misskey.example.com/files/jkl.pdf", Name: "doc.pdf"},
		{Type: "text/plain", URL: "https://misskey.example.com/files/mno.txt", Name: "note.txt"},
	}

	atts := noteAttachments(files)
	if len(atts) != len(files) {
		t.Fatalf("noteAttachments len = %d, want %d", len(atts), len(files))
	}

	wantTypes := []string{
		core.AttachmentTypeImage,
		core.AttachmentTypeAudio,
		core.AttachmentTypeVideo,
		core.AttachmentTypeFile,
		core.AttachmentTypeFile,
	}
	for i, att := range atts {
		if att.Type != wantTypes[i] {
			t.Errorf("att[%d].Type = %q, want %q", i, att.Type, wantTypes[i])
		}
		if att.MimeType != files[i].Type {
			t.Errorf("att[%d].MimeType = %q, want %q", i, att.MimeType, files[i].Type)
		}
		if att.URL != files[i].URL {
			t.Errorf("att[%d].URL = %q, want %q", i, att.URL, files[i].URL)
		}
		if att.Filename != files[i].Name {
			t.Errorf("att[%d].Filename = %q, want %q", i, att.Filename, files[i].Name)
		}
	}

	// 多模态（image/audio/video）必须被下游识别为可消费附件，file 类型不算。
	multimodalCount := 0
	for _, att := range atts {
		if core.IsMultimodalType(att.Type) {
			multimodalCount++
		}
	}
	if multimodalCount != 3 {
		t.Errorf("multimodal attachment count = %d, want 3", multimodalCount)
	}

	// DataURI 应优先返回 URL（云端模型可直接 fetch），不内联字节。
	if uri := atts[0].DataURI(); uri != files[0].URL {
		t.Errorf("DataURI() = %q, want URL %q", uri, files[0].URL)
	}

	// 写入 metadata 后，下游 GetAttachments 应能取出，且多模态标记成立。
	msg := core.Message{Metadata: map[string]any{}}
	core.SetAttachments(&msg, atts)
	if got := core.GetAttachments(&msg); len(got) != len(atts) {
		t.Errorf("GetAttachments len = %d, want %d", len(got), len(atts))
	}
	if !core.HasMultimodalAttachments(&msg) {
		t.Error("HasMultimodalAttachments = false, want true")
	}

	// 无文件时返回 nil（不写空附件，避免下游误判）。
	if got := noteAttachments(nil); got != nil {
		t.Errorf("noteAttachments(nil) = %v, want nil", got)
	}
}

// TestAttachmentTypeFromMIME 锁定 MIME → Type 枚举映射。
func TestAttachmentTypeFromMIME(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/jpeg", core.AttachmentTypeImage},
		{"image/gif", core.AttachmentTypeImage},
		{"audio/ogg", core.AttachmentTypeAudio},
		{"video/webm", core.AttachmentTypeVideo},
		{"application/zip", core.AttachmentTypeFile},
		{"", core.AttachmentTypeFile},
	}
	for _, tc := range cases {
		if got := attachmentTypeFromMIME(tc.mime); got != tc.want {
			t.Errorf("attachmentTypeFromMIME(%q) = %q, want %q", tc.mime, got, tc.want)
		}
	}
}
