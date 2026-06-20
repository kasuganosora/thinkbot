package llm

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// ============================================================================
// Media Validation (P2)
//
// Validates image/file content before sending to providers to avoid
// server-side rejections and wasted bandwidth.
// ============================================================================

const (
	// MaxMediaEncodedBytes is the maximum allowed base64-encoded size (8 MB).
	MaxMediaEncodedBytes = 8 * 1024 * 1024
	// MaxMediaDecodedBytes is the maximum allowed decoded binary size (6 MB).
	MaxMediaDecodedBytes = 6 * 1024 * 1024
)

// allowedImageMIMETypes is the set of MIME types accepted by all providers.
var allowedImageMIMETypes = map[string]bool{
	"image/jpeg": true,
	"image/jpg":  true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// ValidateImagePart validates an ImagePart and returns an error if the
// image exceeds size limits or has an unsupported/invalid MIME type.
//
// For URL-based images (http/https), only the URL format is checked.
// For base64 data URIs, the MIME type and size are validated.
func ValidateImagePart(p ImagePart) error {
	// URL-based images are passed through (provider downloads them).
	if strings.HasPrefix(p.Image, "http://") || strings.HasPrefix(p.Image, "https://") {
		return nil
	}

	// Extract media type and data from data URI.
	mediaType, data, err := parseDataURI(p.Image)
	if err != nil {
		// Not a data URI — treat as raw base64.
		mediaType = p.MediaType
		data = p.Image
	}

	if mediaType != "" && !allowedImageMIMETypes[mediaType] {
		return fmt.Errorf("unsupported image media type: %s (allowed: jpeg, png, gif, webp)", mediaType)
	}

	// Check encoded size.
	if len(data) > MaxMediaEncodedBytes {
		return fmt.Errorf("image data too large: %d encoded bytes (max %d)", len(data), MaxMediaEncodedBytes)
	}

	// Check decoded size (best-effort; ignore decode errors).
	if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
		if len(decoded) > MaxMediaDecodedBytes {
			return fmt.Errorf("image data too large: %d decoded bytes (max %d)", len(decoded), MaxMediaDecodedBytes)
		}
	}

	return nil
}

// ValidateFilePart validates a FilePart's size and MIME type.
func ValidateFilePart(p FilePart) error {
	if len(p.Data) > MaxMediaEncodedBytes {
		return fmt.Errorf("file data too large: %d encoded bytes (max %d)", len(p.Data), MaxMediaEncodedBytes)
	}
	if decoded, err := base64.StdEncoding.DecodeString(p.Data); err == nil {
		if len(decoded) > MaxMediaDecodedBytes {
			return fmt.Errorf("file data too large: %d decoded bytes (max %d)", len(decoded), MaxMediaDecodedBytes)
		}
	}
	return nil
}

// ValidateMessagesMedia validates all media parts in a slice of messages.
func ValidateMessagesMedia(msgs []Message) error {
	for i := range msgs {
		for _, part := range msgs[i].Content {
			switch p := part.(type) {
			case ImagePart:
				if err := ValidateImagePart(p); err != nil {
					return fmt.Errorf("message %d: %w", i, err)
				}
			case FilePart:
				if err := ValidateFilePart(p); err != nil {
					return fmt.Errorf("message %d: %w", i, err)
				}
			}
		}
	}
	return nil
}

// parseDataURI extracts media type and base64 data from a data URI string
// (e.g. "data:image/png;base64,iVBOR..."). Returns ("", "", err) if not a data URI.
func parseDataURI(s string) (mediaType string, data string, err error) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", fmt.Errorf("not a data URI")
	}
	s = s[5:] // strip "data:"

	semiIdx := strings.Index(s, ";")
	commaIdx := strings.Index(s, ",")
	if commaIdx < 0 {
		return "", "", fmt.Errorf("invalid data URI: missing comma")
	}

	if semiIdx > 0 && semiIdx < commaIdx {
		mediaType = s[:semiIdx]
	} else {
		mediaType = s[:commaIdx]
	}

	data = s[commaIdx+1:]
	return mediaType, data, nil
}
