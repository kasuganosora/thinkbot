package google

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// ============================================================================
// 图片生成
// ============================================================================

// ImageGenerationOptions 图片生成选项。
type ImageGenerationOptions struct {
	// AspectRatio 宽高比（如 AspectRatio16_9）。空字符串 = 默认。
	AspectRatio string
	// ImageSize 分辨率（如 ImageSize1K、ImageSize2K）。空字符串 = 默认。
	ImageSize string
	// ResponseModalities 响应模态。默认 ["TEXT", "IMAGE"]。
	ResponseModalities []ResponseModality
	// ThinkingConfig 思考配置（仅 gemini-3.1-flash-image 支持）。
	ThinkingConfig *ThinkingConfig
}

// GenerateImage 发送图片生成请求。
//
// model 应为图片生成模型（如 ModelGemini31FlashImage、ModelGemini25FlashImage）。
// prompt 为文本提示。
// opts 为生成选项（宽高比、分辨率等）。
//
// 返回的 GenerateContentResponse 中，candidates[].content.parts[] 可能包含
// TextPart（文本描述）和 InlineDataPart（base64 编码的 PNG 图片）。
//
// 使用 ExtractImages 从响应中提取图片数据。
func (c *Client) GenerateImage(ctx context.Context, model, prompt string, opts *ImageGenerationOptions) (*GenerateContentResponse, error) {
	if model == "" {
		return nil, errors.New("google: model is required")
	}

	req := GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart(prompt)},
		}},
	}

	genCfg := &GenerationConfig{}
	if opts != nil {
		modalities := opts.ResponseModalities
		if len(modalities) == 0 {
			modalities = []ResponseModality{ModalityText, ModalityImage}
		}
		genCfg.ResponseModalities = modalities

		if opts.AspectRatio != "" || opts.ImageSize != "" {
			genCfg.ResponseFormat = &ResponseFormat{
				Image: &ImageResponseFormat{
					AspectRatio: opts.AspectRatio,
					ImageSize:   opts.ImageSize,
				},
			}
		}
		genCfg.ThinkingConfig = opts.ThinkingConfig
	} else {
		genCfg.ResponseModalities = []ResponseModality{ModalityText, ModalityImage}
	}
	req.GenerationConfig = genCfg

	return c.GenerateContent(ctx, model, req)
}

// ============================================================================
// 图片编辑（图生图）
// ============================================================================

// EditImage 通过参考图片和编辑指令生成新图片。
//
// model 应为图片生成模型。
// prompt 为编辑指令。
// referenceImages 为参考图片（base64 内联数据或 File URI 引用）。
// opts 为生成选项。
func (c *Client) EditImage(ctx context.Context, model, prompt string, referenceImages []Part, opts *ImageGenerationOptions) (*GenerateContentResponse, error) {
	if model == "" {
		return nil, errors.New("google: model is required")
	}
	if len(referenceImages) == 0 {
		return nil, errors.New("google: at least one reference image is required")
	}

	parts := make([]Part, 0, len(referenceImages)+1)
	parts = append(parts, TextPart(prompt))
	parts = append(parts, referenceImages...)

	req := GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: parts,
		}},
	}

	genCfg := &GenerationConfig{}
	if opts != nil {
		modalities := opts.ResponseModalities
		if len(modalities) == 0 {
			modalities = []ResponseModality{ModalityText, ModalityImage}
		}
		genCfg.ResponseModalities = modalities

		if opts.AspectRatio != "" || opts.ImageSize != "" {
			genCfg.ResponseFormat = &ResponseFormat{
				Image: &ImageResponseFormat{
					AspectRatio: opts.AspectRatio,
					ImageSize:   opts.ImageSize,
				},
			}
		}
		genCfg.ThinkingConfig = opts.ThinkingConfig
	} else {
		genCfg.ResponseModalities = []ResponseModality{ModalityText, ModalityImage}
	}
	req.GenerationConfig = genCfg

	return c.GenerateContent(ctx, model, req)
}

// ============================================================================
// 响应提取工具
// ============================================================================

// ExtractImages 从 generateContent 响应中提取所有图片数据。
//
// 返回 (图片列表, 文本列表)：
//   - 图片列表: 每个 Blob 包含 MimeType 和 base64 编码的 Data
//   - 文本列表: 响应中的所有文本内容（不含思考摘要）
func ExtractImages(resp *GenerateContentResponse) ([]Blob, []string) {
	if resp == nil {
		return nil, nil
	}

	var images []Blob
	var texts []string

	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && isImageMIME(part.InlineData.MimeType) {
				images = append(images, *part.InlineData)
			}
			if part.Text != "" && !part.Thought {
				texts = append(texts, part.Text)
			}
		}
	}

	return images, texts
}

// ExtractMedia 从 generateContent 响应中提取所有内联媒体数据（图片、音频、视频）。
//
// 返回所有非空非文本的 InlineData Part。
func ExtractMedia(resp *GenerateContentResponse) []Blob {
	if resp == nil {
		return nil
	}

	var media []Blob
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				media = append(media, *part.InlineData)
			}
		}
	}
	return media
}

// ExtractThoughtSignatures 从响应中提取思考签名。
//
// 多轮图片编辑对话中，需要将上一轮的思考签名回传给模型。
func ExtractThoughtSignatures(resp *GenerateContentResponse) []Part {
	if resp == nil {
		return nil
	}

	var parts []Part
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.ThoughtSignature != "" {
				parts = append(parts, part)
			}
		}
	}
	return parts
}

// ============================================================================
// Base64 编码辅助
// ============================================================================

// EncodeBase64 将字节数据编码为 base64 字符串（标准编码）。
func EncodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// DecodeBase64 将 base64 字符串解码为字节数据。
func DecodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// EncodeFileToBase64 从文件路径读取文件并返回 base64 编码字符串。
func EncodeFileToBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return EncodeBase64(data), nil
}

// EncodeReaderToBase64 从 reader 读取数据并返回 base64 编码字符串。
func EncodeReaderToBase64(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return EncodeBase64(data), nil
}

// SaveImageToFile 将 base64 编码的图片数据保存到文件。
//
// 适用于从 GenerateImage 响应中提取的图片 Blob。
func SaveImageToFile(blob Blob, path string) error {
	data, err := DecodeBase64(blob.Data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ============================================================================
// 内部工具
// ============================================================================

// isImageMIME 判断 MIME 类型是否为图片。
func isImageMIME(mimeType string) bool {
	switch mimeType {
	case MIMEPNG, MIMEJPEG, MIMEWEBP, MIMEHEIC, MIMEHEIF:
		return true
	default:
		return false
	}
}
