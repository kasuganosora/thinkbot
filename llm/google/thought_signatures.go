package google

import "fmt"

// ============================================================================
// 思考签名（Thought Signatures）
//
// 思考签名是模型内部思考过程的加密表示，用于在多步交互中保留推理上下文。
//
// 核心规则：
//   - Gemini 3 模型在函数调用时**强制要求**传回 thoughtSignature，否则返回 400
//   - 并行函数调用时，签名仅附加到第一个 FunctionCall 部分
//   - 顺序函数调用时，每个 FunctionCall 部分都需要签名
//   - 非函数调用部分（如最后的 text）的签名是可选的但建议保留
//
// 用法示例（多轮函数调用）：
//
//	// 1. 发送请求
//	resp, _ := client.GenerateContent(ctx, model, req)
//
//	// 2. 提取模型响应（保留签名）
//	modelContent := google.PreserveModelContent(resp)
//
//	// 3. 构建下一轮请求
//	nextReq := google.GenerateContentRequest{
//	    Contents: []Content{
//	        originalUserContent,
//	        *modelContent,
//	        {Role: google.RoleUser, Parts: functionResponseParts},
//	    },
//	}
//
//	// 4. （可选）验证签名
//	if err := google.ValidateFunctionCallSignatures(nextReq.Contents); err != nil {
//	    return err
//	}
//
//	resp2, _ := client.GenerateContent(ctx, model, nextReq)
// ============================================================================

// ThoughtSignatureEntry 表示一条思考签名信息。
type ThoughtSignatureEntry struct {
	// PartIndex 在候选内容中的 parts 数组索引。
	PartIndex int

	// Signature 签名值。
	Signature string

	// FunctionName 如果签名附加在 FunctionCall 上，则为函数名；否则为空。
	FunctionName string
}

// ExtractThoughtSignatureEntries 从响应中提取所有思考签名条目。
//
// 返回每个携带 thoughtSignature 的 Part 的详细信息，
// 包括其在 parts 数组中的索引、签名值和关联的函数名（如果有）。
func ExtractThoughtSignatureEntries(resp *GenerateContentResponse) []ThoughtSignatureEntry {
	if resp == nil {
		return nil
	}

	var entries []ThoughtSignatureEntry
	for _, cand := range resp.Candidates {
		for i, part := range cand.Content.Parts {
			if part.ThoughtSignature != "" {
				entry := ThoughtSignatureEntry{
					PartIndex: i,
					Signature: part.ThoughtSignature,
				}
				if part.FunctionCall != nil {
					entry.FunctionName = part.FunctionCall.Name
				}
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// ExtractFirstFunctionCallSignature 从响应中提取第一个 FunctionCall 部分的思考签名。
//
// 并行函数调用时，签名仅附加到第一个 FunctionCall 部分。
// 返回空字符串表示未找到。
func ExtractFirstFunctionCallSignature(resp *GenerateContentResponse) string {
	if resp == nil {
		return ""
	}

	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil && part.ThoughtSignature != "" {
				return part.ThoughtSignature
			}
		}
	}
	return ""
}

// ExtractLastTextSignature 从响应中提取最后一个非 FunctionCall 部分的思考签名。
//
// 模型返回的最后内容部分（如 text）可能携带签名，建议在下一轮中传回。
// 返回空字符串表示未找到。
func ExtractLastTextSignature(resp *GenerateContentResponse) string {
	if resp == nil {
		return ""
	}

	for _, cand := range resp.Candidates {
		for i := len(cand.Content.Parts) - 1; i >= 0; i-- {
			part := cand.Content.Parts[i]
			if part.FunctionCall == nil && part.FunctionResponse == nil && part.ThoughtSignature != "" {
				return part.ThoughtSignature
			}
		}
	}
	return ""
}

// MissingSignatureError 表示模型消息中的 FunctionCall 部分缺少思考签名。
//
// Gemini 3 模型在函数调用期间必须传回思考签名，否则会收到 400 验证错误。
type MissingSignatureError struct {
	ContentIndex int
	PartIndex    int
	FunctionName string
}

func (e *MissingSignatureError) Error() string {
	if e.FunctionName != "" {
		return fmt.Sprintf("google: function call %q at contents[%d].parts[%d] is missing thoughtSignature", e.FunctionName, e.ContentIndex, e.PartIndex)
	}
	return fmt.Sprintf("google: function call at contents[%d].parts[%d] is missing thoughtSignature", e.ContentIndex, e.PartIndex)
}

// ValidateFunctionCallSignatures 验证所有 model 消息中的 FunctionCall 部分都有思考签名。
//
// Gemini 3 模型在函数调用期间强制要求传回思考签名。
// 并行函数调用中，签名只需附加到第一个 FunctionCall 部分。
//
// 返回 nil 表示验证通过，返回 *MissingSignatureError 表示缺少签名。
func ValidateFunctionCallSignatures(contents []Content) error {
	for ci, content := range contents {
		if content.Role != RoleModel {
			continue
		}
		// 对于每个 model 消息，检查第一个 FunctionCall 部分是否有签名
		foundFC := false
		for pi, part := range content.Parts {
			if part.FunctionCall == nil {
				continue
			}
			// 第一个 FunctionCall 必须有签名
			if !foundFC {
				foundFC = true
				if part.ThoughtSignature == "" {
					return &MissingSignatureError{
						ContentIndex: ci,
						PartIndex:    pi,
						FunctionName: part.FunctionCall.Name,
					}
				}
			}
		}
	}
	return nil
}

// PreserveModelContent 从响应中提取模型内容，保留所有思考签名。
//
// 这是在多轮函数调用对话中构建历史记录的最简单方式：
//
//	resp, _ := client.GenerateContent(ctx, model, req)
//	modelContent := PreserveModelContent(resp)
//	// 将 modelContent 追加到下一轮请求的 Contents 中
//
// 返回 nil 表示响应中没有候选内容。
func PreserveModelContent(resp *GenerateContentResponse) *Content {
	if resp == nil || len(resp.Candidates) == 0 {
		return nil
	}

	cand := resp.Candidates[0]
	content := Content{
		Role:  RoleModel,
		Parts: make([]Part, len(cand.Content.Parts)),
	}
	copy(content.Parts, cand.Content.Parts)
	return &content
}

// AttachSignatureToFunctionCall 将思考签名附加到 Content 中第一个 FunctionCall 部分。
//
// 用于手动构建对话历史时，将之前响应中的签名回填到 FunctionCall 部分。
// 返回 true 表示成功附加，false 表示未找到 FunctionCall 部分。
func AttachSignatureToFunctionCall(content *Content, signature string) bool {
	if content == nil {
		return false
	}
	for i := range content.Parts {
		if content.Parts[i].FunctionCall != nil && content.Parts[i].ThoughtSignature == "" {
			content.Parts[i].ThoughtSignature = signature
			return true
		}
	}
	return false
}

// AttachSignaturesByPosition 将思考签名按 parts 索引批量附加到 Content 中。
//
// entries 通常来自 ExtractThoughtSignatureEntries。
// 对于并行函数调用，签名仅存在于第一个 FC 部分，此函数会自动匹配。
func AttachSignaturesByPosition(content *Content, entries []ThoughtSignatureEntry) {
	if content == nil || len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		if entry.PartIndex >= 0 && entry.PartIndex < len(content.Parts) {
			content.Parts[entry.PartIndex].ThoughtSignature = entry.Signature
		}
	}
}

// BuildFunctionResponseTurn 构建函数响应轮次的对话内容。
//
// 这是一个便捷函数，用于在多轮函数调用中：
//  1. 将模型响应（保留签名）作为 model 消息
//  2. 将函数响应作为 user 消息
//
// 参数：
//   - resp: 上一轮的 generateContent 响应（签名会从中提取）
//   - functionResponses: 函数执行结果的 Parts（使用 FunctionResponsePart 创建）
//
// 返回两段 Content（model 消息 + user 消息），可直接追加到 Contents 中。
// 如果 resp 中没有候选内容，返回 nil。
func BuildFunctionResponseTurn(resp *GenerateContentResponse, functionResponses []Part) []Content {
	modelContent := PreserveModelContent(resp)
	if modelContent == nil {
		return nil
	}

	return []Content{
		*modelContent,
		{
			Role:  RoleUser,
			Parts: functionResponses,
		},
	}
}

// StripThoughtSignatures 移除所有内容中的思考签名。
//
// 用于长时间对话中减少 token 用量（清除之前轮次的签名）。
//
// 注意：切勿清除当前轮次中的思考签名，否则会导致 400 验证错误。
// 此函数应仅用于清除历史轮次的签名。
func StripThoughtSignatures(contents []Content) {
	for ci := range contents {
		for pi := range contents[ci].Parts {
			contents[ci].Parts[pi].ThoughtSignature = ""
		}
	}
}

// StripOldTurnSignatures 移除指定轮次之前的所有思考签名。
//
// currentTurnStartIndex 为当前轮次开始的内容索引（最近的 user 文本消息）。
// 该索引及之后的内容中的签名会保留，之前的会被清除。
//
// 用于长时间对话中减少 token 用量。
func StripOldTurnSignatures(contents []Content, currentTurnStartIndex int) {
	for ci := range contents {
		if ci < currentTurnStartIndex {
			for pi := range contents[ci].Parts {
				contents[ci].Parts[pi].ThoughtSignature = ""
			}
		}
	}
}
