package anthropic

import (
	"fmt"
	"math"
)

// ============================================================================
// 图片内容块构造函数
// ============================================================================

// Base64ImageBlock 从 base64 编码数据创建图片 ContentBlock。
func Base64ImageBlock(mediaType, data string) ContentBlock {
	return ContentBlock{
		Type:   ContentTypeImage,
		Source: Base64ImageSource(mediaType, data),
	}
}

// URLImageBlock 从 URL 创建图片 ContentBlock。
func URLImageBlock(url string) ContentBlock {
	return ContentBlock{
		Type:   ContentTypeImage,
		Source: URLImageSource(url),
	}
}

// FileImageBlock 从 Files API file_id 创建图片 ContentBlock。
func FileImageBlock(fileID string) ContentBlock {
	return ContentBlock{
		Type:   ContentTypeImage,
		Source: FileImageSource(fileID),
	}
}

// ============================================================================
// 视觉消息内容辅助函数
// ============================================================================

// ImageWithText 创建一个图片块后跟文本块的 MessageContent。
//
// 推荐将图片放在文本之前，以获得最佳效果。
func ImageWithText(image ContentBlock, text string) MessageContent {
	return MessageContent{
		image,
		{Type: ContentTypeText, Text: text},
	}
}

// MultiImageContent 创建包含多张标注图片和一段文本的 MessageContent。
//
// 生成的结构为："Image 1: [img1] Image 2: [img2] ... {text}"。
// 适用于对比、差异描述等多图场景。
func MultiImageContent(text string, images ...ContentBlock) MessageContent {
	var content MessageContent
	for i, img := range images {
		content = append(content, ContentBlock{
			Type: ContentTypeText,
			Text: fmt.Sprintf("Image %d:", i+1),
		})
		content = append(content, img)
	}
	if text != "" {
		content = append(content, ContentBlock{Type: ContentTypeText, Text: text})
	}
	return content
}

// ============================================================================
// PDF 文档内容块构造函数
// ============================================================================

// PDF MIME 类型。
const MimeTypePDF = "application/pdf"

// Base64PDFSource 创建 base64 编码的 PDF 文档来源。
func Base64PDFSource(data string) *ImageSource {
	return &ImageSource{Type: "base64", MediaType: MimeTypePDF, Data: data}
}

// Base64DocumentBlock 从 base64 编码数据创建 PDF 文档 ContentBlock。
func Base64DocumentBlock(data string) ContentBlock {
	return ContentBlock{
		Type:   ContentTypeDocument,
		Source: Base64PDFSource(data),
	}
}

// DocumentWithText 创建一个 PDF 文档块后跟文本块的 MessageContent。
func DocumentWithText(pdfData string, text string) MessageContent {
	return MessageContent{
		Base64DocumentBlock(pdfData),
		{Type: ContentTypeText, Text: text},
	}
}

// ============================================================================
// Thinking 内容块构造函数
// ============================================================================

// ThinkingBlock 创建一个 thinking 类型的内容块。
//
// 用于多轮对话中回传上一轮的扩展思考结果。
// Signature 是服务端返回的签名，必须原样传回。
func ThinkingBlock(thinking, signature string) ContentBlock {
	return ContentBlock{
		Type:      ContentTypeThinking,
		Thinking:  thinking,
		Signature: signature,
	}
}

// ============================================================================
// 图片 token 计算与尺寸工具
// ============================================================================

// 标准分辨率限制（大多数模型）。
const (
	ImageMaxEdgeStandard   = 1568
	ImageMaxTokensStandard = 1568
)

// 高分辨率限制（Claude Opus 4.7 及更高版本）。
const (
	ImageMaxEdgeHighRes   = 2576
	ImageMaxTokensHighRes = 4784
)

// 视觉 token 补丁大小（28×28 像素）。
const ImagePatchSize = 28

// CountImageTokens 返回图片消耗的视觉 token 数：每个 28×28 像素补丁一个 token。
func CountImageTokens(width, height int) int {
	return ceilDiv(width, ImagePatchSize) * ceilDiv(height, ImagePatchSize)
}

// ResizedSize 计算 Claude 在填充前将图片缩放到的尺寸。
//
// maxEdge 为单边最大像素数，maxTokens 为视觉 token 预算。
// 对于大多数模型使用 ImageMaxEdgeStandard / ImageMaxTokensStandard；
// 对于 Claude Opus 4.7 及更高版本使用 ImageMaxEdgeHighRes / ImageMaxTokensHighRes。
//
// 已经在限制范围内的图片将原样返回。
func ResizedSize(width, height, maxEdge, maxTokens int) (int, int) {
	fits := func(w, h int) bool {
		return ceilDiv(w, ImagePatchSize)*ImagePatchSize <= maxEdge &&
			ceilDiv(h, ImagePatchSize)*ImagePatchSize <= maxEdge &&
			CountImageTokens(w, h) <= maxTokens
	}

	if fits(width, height) {
		return width, height
	}
	if height > width {
		rh, rw := ResizedSize(height, width, maxEdge, maxTokens)
		return rw, rh
	}

	aspectRatio := float64(width) / float64(height)
	lo, hi := 1, width
	for lo+1 < hi {
		mid := (lo + hi) / 2
		h := max(int(math.Round(float64(mid)/aspectRatio)), 1)
		if fits(mid, h) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, max(int(math.Round(float64(lo)/aspectRatio)), 1)
}

// ResizedSizeStandard 计算标准分辨率模型（大多数模型）的缩放尺寸。
func ResizedSizeStandard(width, height int) (int, int) {
	return ResizedSize(width, height, ImageMaxEdgeStandard, ImageMaxTokensStandard)
}

// ResizedSizeHighRes 计算高分辨率模型（Claude Opus 4.7+）的缩放尺寸。
func ResizedSizeHighRes(width, height int) (int, int) {
	return ResizedSize(width, height, ImageMaxEdgeHighRes, ImageMaxTokensHighRes)
}

// ToRelativeCoordinates 将 Claude 返回的像素坐标映射到 [0, 1] 的相对坐标。
//
// originalWidth/originalHeight 应为你上传图片的实际像素尺寸。
// 对于 Claude Opus 4.7 及更高版本，使用 maxEdge=2576, maxTokens=4784。
//
// 若要将相对坐标映射回原始图片像素空间，乘以原始尺寸即可：
//
//	relX, relY := ToRelativeCoordinates(x, y, origW, origH, ImageMaxEdgeStandard, ImageMaxTokensStandard)
//	origX, origY := relX * float64(origW), relY * float64(origH)
func ToRelativeCoordinates(x, y float64, originalWidth, originalHeight, maxEdge, maxTokens int) (float64, float64) {
	rw, rh := ResizedSize(originalWidth, originalHeight, maxEdge, maxTokens)
	return x / float64(rw), y / float64(rh)
}

// ceilDiv 向上取整除法。
func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}
