package core

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// ============================================================================
// Attachment — 多模态附件类型
//
// Channel 在收到带媒体的消息时，将附件信息写入 Message.Metadata["attachments"]。
// 格式为 []Attachment。下游 Stage（如 MultimodalStage）可据此判断消息是否
// 包含多模态内容并进行相应处理。
// ============================================================================

// Attachment 表示消息中的一个多模态附件。
type Attachment struct {
	// Type 附件类型："image"、"audio"、"video"、"file"。
	Type string `json:"type"`

	// MimeType MIME 类型（如 "image/png"、"audio/mp3"）。
	MimeType string `json:"mimeType,omitempty"`

	// URL 附件的公开可访问 URL 或 data URI。
	// 优先使用 URL（避免传输大量数据）。
	URL string `json:"url,omitempty"`

	// Data 附件原始字节（URL 为空时使用）。
	Data []byte `json:"data,omitempty"`

	// Filename 文件名（可选）。
	Filename string `json:"filename,omitempty"`
}

const (
	// AttachmentTypeImage 图片附件。
	AttachmentTypeImage string = "image"
	// AttachmentTypeAudio 音频附件。
	AttachmentTypeAudio string = "audio"
	// AttachmentTypeVideo 视频附件。
	AttachmentTypeVideo string = "video"
	// AttachmentTypeFile 其他文件附件。
	AttachmentTypeFile string = "file"
)

const attachmentsKey = "attachments"

// GetAttachments 从 Message.Metadata 中提取附件列表。
// 如果没有附件，返回 nil。
func GetAttachments(msg *Message) []Attachment {
	if msg.Metadata == nil {
		return nil
	}
	raw, ok := msg.Metadata[attachmentsKey]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []Attachment:
		return v
	case []any:
		result := make([]Attachment, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				result = append(result, attachmentFromMap(m))
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return nil
}

// SetAttachments 将附件列表写入 Message.Metadata。
func SetAttachments(msg *Message, attachments []Attachment) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[attachmentsKey] = attachments
}

// HasMultimodalAttachments 检查消息是否包含多模态附件（image/audio/video）。
func HasMultimodalAttachments(msg *Message) bool {
	for _, a := range GetAttachments(msg) {
		if IsMultimodalType(a.Type) {
			return true
		}
	}
	return false
}

// IsMultimodalType 判断附件类型是否为多模态（image/audio/video）。
// "file" 类型不算多模态（普通文件，需要文字描述）。
func IsMultimodalType(typ string) bool {
	switch typ {
	case AttachmentTypeImage, AttachmentTypeAudio, AttachmentTypeVideo:
		return true
	}
	return false
}

// IsImageType 判断是否为图片类型。
func IsImageType(typ string) bool {
	return typ == AttachmentTypeImage || strings.HasPrefix(typ, "image/")
}

// IsAudioType 判断是否为音频类型。
func IsAudioType(typ string) bool {
	return typ == AttachmentTypeAudio || strings.HasPrefix(typ, "audio/")
}

// IsVideoType 判断是否为视频类型。
func IsVideoType(typ string) bool {
	return typ == AttachmentTypeVideo || strings.HasPrefix(typ, "video/")
}

// DataURI 返回附件的 data URI（base64 编码）。
// 如果已有 URL 且是公开可访问的，直接返回 URL。
// 如果只有 Data，则构造 data URI。
func (a Attachment) DataURI() string {
	if a.URL != "" {
		return a.URL
	}
	if len(a.Data) > 0 {
		mime := a.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(a.Data))
	}
	return ""
}

// IsDataURL 检查字符串是否为 data: URI。
func IsDataURL(s string) bool { return strings.HasPrefix(s, "data:") }

// IsHTTPURL 检查字符串是否为 http(s) URL。
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// attachmentFromMap 从 map[string]any 构造 Attachment（用于 JSON 反序列化兼容）。
func attachmentFromMap(m map[string]any) Attachment {
	a := Attachment{}
	if v, ok := m["type"].(string); ok {
		a.Type = v
	}
	if v, ok := m["mimeType"].(string); ok {
		a.MimeType = v
	}
	if v, ok := m["url"].(string); ok {
		a.URL = v
	}
	if v, ok := m["filename"].(string); ok {
		a.Filename = v
	}
	return a
}
