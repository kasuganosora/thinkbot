package llm

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateImagePart_URL(t *testing.T) {
	p := ImagePart{Image: "https://example.com/image.png"}
	if err := ValidateImagePart(p); err != nil {
		t.Errorf("URL image should pass: %v", err)
	}
}

func TestValidateImagePart_ValidBase64(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("fake-image-data"))
	p := ImagePart{Image: data, MediaType: "image/png"}
	if err := ValidateImagePart(p); err != nil {
		t.Errorf("valid base64 image should pass: %v", err)
	}
}

func TestValidateImagePart_UnsupportedMIME(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("fake"))
	p := ImagePart{Image: data, MediaType: "image/bmp"}
	if err := ValidateImagePart(p); err == nil {
		t.Error("unsupported MIME type should fail")
	}
}

func TestValidateImagePart_TooLarge(t *testing.T) {
	// Create data larger than MaxMediaEncodedBytes
	large := strings.Repeat("A", MaxMediaEncodedBytes+1)
	p := ImagePart{Image: large, MediaType: "image/png"}
	if err := ValidateImagePart(p); err == nil {
		t.Error("oversized image should fail")
	}
}

func TestValidateImagePart_DataURI(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("fake"))
	p := ImagePart{Image: "data:image/png;base64," + data}
	if err := ValidateImagePart(p); err != nil {
		t.Errorf("valid data URI should pass: %v", err)
	}
}

func TestValidateFilePart_OK(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("file-contents"))
	p := FilePart{Data: data, MediaType: "application/pdf"}
	if err := ValidateFilePart(p); err != nil {
		t.Errorf("valid file should pass: %v", err)
	}
}

func TestValidateFilePart_TooLarge(t *testing.T) {
	large := strings.Repeat("A", MaxMediaEncodedBytes+1)
	p := FilePart{Data: large}
	if err := ValidateFilePart(p); err == nil {
		t.Error("oversized file should fail")
	}
}

func TestValidateMessagesMedia(t *testing.T) {
	msgs := []Message{
		{
			Role: MessageRoleUser,
			Content: []MessagePart{
				ImagePart{Image: "https://example.com/img.png"},
				TextPart{Text: "look at this"},
			},
		},
	}
	if err := ValidateMessagesMedia(msgs); err != nil {
		t.Errorf("valid messages should pass: %v", err)
	}

	badMsgs := []Message{
		{
			Role: MessageRoleUser,
			Content: []MessagePart{
				ImagePart{Image: "data:image/bmp;base64,ZmFrZQ==", MediaType: "image/bmp"},
			},
		},
	}
	if err := ValidateMessagesMedia(badMsgs); err == nil {
		t.Error("invalid MIME should fail")
	}
}

func TestParseDataURI(t *testing.T) {
	// Valid data URI
	mt, data, err := parseDataURI("data:image/png;base64,abc123")
	if err != nil {
		t.Fatalf("expected no error: %v", err)
	}
	if mt != "image/png" {
		t.Errorf("expected image/png, got %s", mt)
	}
	if data != "abc123" {
		t.Errorf("expected abc123, got %s", data)
	}

	// Not a data URI
	_, _, err = parseDataURI("https://example.com/image.png")
	if err == nil {
		t.Error("expected error for non-data-URI")
	}
}
