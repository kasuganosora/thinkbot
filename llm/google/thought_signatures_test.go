package google

import (
	"encoding/json"
	"testing"
)

// ============================================================================
// Part 构造器测试
// ============================================================================

func TestFunctionCallPartWithSignature(t *testing.T) {
	args := map[string]any{"city": "Paris"}
	p := FunctionCallPartWithSignature("get_weather", "sig_ABC", args)

	if p.FunctionCall == nil {
		t.Fatal("FunctionCall is nil")
	}
	if p.FunctionCall.Name != "get_weather" {
		t.Errorf("name = %q, want get_weather", p.FunctionCall.Name)
	}
	if p.ThoughtSignature != "sig_ABC" {
		t.Errorf("signature = %q, want sig_ABC", p.ThoughtSignature)
	}
	if p.FunctionCall.Args["city"] != "Paris" {
		t.Errorf("args[city] = %v, want Paris", p.FunctionCall.Args["city"])
	}
}

func TestFunctionCallPartWithIDAndSignature(t *testing.T) {
	p := FunctionCallPartWithIDAndSignature("check_flight", "fc-1", "sig_X", map[string]any{"flight": "AA100"})

	if p.FunctionCall == nil || p.FunctionCall.ID != "fc-1" {
		t.Fatal("missing function call ID")
	}
	if p.ThoughtSignature != "sig_X" {
		t.Errorf("signature = %q, want sig_X", p.ThoughtSignature)
	}
}

func TestTextPartWithSignature(t *testing.T) {
	p := TextPartWithSignature("Hello", "sig_T")

	if p.Text != "Hello" {
		t.Errorf("text = %q, want Hello", p.Text)
	}
	if p.ThoughtSignature != "sig_T" {
		t.Errorf("signature = %q, want sig_T", p.ThoughtSignature)
	}
	if p.Thought {
		t.Error("should not be a thought part")
	}
}

func TestSignaturePart(t *testing.T) {
	p := SignaturePart("sig_S")

	if p.ThoughtSignature != "sig_S" {
		t.Errorf("signature = %q, want sig_S", p.ThoughtSignature)
	}
	if p.Text != "" {
		t.Errorf("text should be empty, got %q", p.Text)
	}
	if p.FunctionCall != nil {
		t.Error("FunctionCall should be nil")
	}
}

func TestDummySignatureConstants(t *testing.T) {
	if ThoughtSignatureSkip != "skip_thought_signature_validator" {
		t.Errorf("ThoughtSignatureSkip = %q", ThoughtSignatureSkip)
	}
	if ThoughtSignatureDummy != "context_engineering_is_the_way_to_go" {
		t.Errorf("ThoughtSignatureDummy = %q", ThoughtSignatureDummy)
	}
}

// ============================================================================
// JSON 序列化/反序列化测试
// ============================================================================

func TestPartWithSignatureJSON(t *testing.T) {
	p := FunctionCallPartWithSignature("get_weather", "sig123", map[string]any{"city": "Tokyo"})

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	// 验证 JSON 包含 thoughtSignature 在 Part 级别
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if _, ok := raw["thoughtSignature"]; !ok {
		t.Error("thoughtSignature should be at Part level in JSON")
	}

	// 验证签名值
	var sig string
	if err := json.Unmarshal(raw["thoughtSignature"], &sig); err != nil {
		t.Fatalf("unmarshal signature error: %v", err)
	}
	if sig != "sig123" {
		t.Errorf("signature = %q, want sig123", sig)
	}
}

func TestPartRoundTripSignature(t *testing.T) {
	original := Part{
		Text:             "result text",
		ThoughtSignature: "sig_roundtrip",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded Part
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ThoughtSignature != original.ThoughtSignature {
		t.Errorf("signature = %q, want %q", decoded.ThoughtSignature, original.ThoughtSignature)
	}
	if decoded.Text != original.Text {
		t.Errorf("text = %q, want %q", decoded.Text, original.Text)
	}
}

// ============================================================================
// ExtractThoughtSignatureEntries 测试
// ============================================================================

func TestExtractThoughtSignatureEntries(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("get_weather", "sig_A", map[string]any{"city": "Paris"}),
					FunctionCallPart("get_weather", map[string]any{"city": "London"}), // 并行调用，无签名
					TextPartWithSignature("Done", "sig_B"),
				},
			},
		}},
	}

	entries := ExtractThoughtSignatureEntries(resp)
	if len(entries) != 2 {
		t.Fatalf("entries count = %d, want 2", len(entries))
	}

	if entries[0].Signature != "sig_A" {
		t.Errorf("entries[0].Signature = %q, want sig_A", entries[0].Signature)
	}
	if entries[0].FunctionName != "get_weather" {
		t.Errorf("entries[0].FunctionName = %q, want get_weather", entries[0].FunctionName)
	}
	if entries[0].PartIndex != 0 {
		t.Errorf("entries[0].PartIndex = %d, want 0", entries[0].PartIndex)
	}

	if entries[1].Signature != "sig_B" {
		t.Errorf("entries[1].Signature = %q, want sig_B", entries[1].Signature)
	}
	if entries[1].FunctionName != "" {
		t.Errorf("entries[1].FunctionName = %q, want empty", entries[1].FunctionName)
	}
}

func TestExtractThoughtSignatureEntriesNil(t *testing.T) {
	if entries := ExtractThoughtSignatureEntries(nil); entries != nil {
		t.Errorf("expected nil, got %v", entries)
	}
}

// ============================================================================
// ExtractFirstFunctionCallSignature 测试
// ============================================================================

func TestExtractFirstFunctionCallSignature(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("get_weather", "sig_first", map[string]any{"city": "Paris"}),
					FunctionCallPart("get_weather", map[string]any{"city": "London"}),
				},
			},
		}},
	}

	sig := ExtractFirstFunctionCallSignature(resp)
	if sig != "sig_first" {
		t.Errorf("signature = %q, want sig_first", sig)
	}
}

func TestExtractFirstFunctionCallSignatureNone(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPart("get_weather", map[string]any{"city": "Paris"}),
				},
			},
		}},
	}

	sig := ExtractFirstFunctionCallSignature(resp)
	if sig != "" {
		t.Errorf("expected empty string, got %q", sig)
	}
}

// ============================================================================
// ExtractLastTextSignature 测试
// ============================================================================

func TestExtractLastTextSignature(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					TextPart("thinking..."),
					TextPartWithSignature("Final answer", "sig_last"),
				},
			},
		}},
	}

	sig := ExtractLastTextSignature(resp)
	if sig != "sig_last" {
		t.Errorf("signature = %q, want sig_last", sig)
	}
}

func TestExtractLastTextSignatureFromEmptyPart(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					TextPart("hello"),
					SignaturePart("sig_empty"),
				},
			},
		}},
	}

	sig := ExtractLastTextSignature(resp)
	if sig != "sig_empty" {
		t.Errorf("signature = %q, want sig_empty", sig)
	}
}

// ============================================================================
// ValidateFunctionCallSignatures 测试
// ============================================================================

func TestValidateFunctionCallSignaturesOK(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("query")}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("get_weather", "sig_A", map[string]any{"city": "Paris"}),
			FunctionCallPart("get_weather", map[string]any{"city": "London"}), // 并行，无需签名
		}},
		{Role: RoleUser, Parts: []Part{
			FunctionResponsePart("get_weather", map[string]any{"temp": "15C"}),
			FunctionResponsePart("get_weather", map[string]any{"temp": "12C"}),
		}},
	}

	if err := ValidateFunctionCallSignatures(contents); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestValidateFunctionCallSignaturesMissing(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("query")}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPart("get_weather", map[string]any{"city": "Paris"}), // 无签名
		}},
	}

	err := ValidateFunctionCallSignatures(contents)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	mse, ok := err.(*MissingSignatureError)
	if !ok {
		t.Fatalf("expected *MissingSignatureError, got %T", err)
	}
	if mse.ContentIndex != 1 {
		t.Errorf("ContentIndex = %d, want 1", mse.ContentIndex)
	}
	if mse.FunctionName != "get_weather" {
		t.Errorf("FunctionName = %q, want get_weather", mse.FunctionName)
	}
}

func TestValidateFunctionCallSignaturesMultipleTurns(t *testing.T) {
	// 顺序调用：每步的 FC 都需要签名
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("query")}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("check_flight", "sig_A", map[string]any{"flight": "AA100"}),
		}},
		{Role: RoleUser, Parts: []Part{
			FunctionResponsePart("check_flight", map[string]any{"status": "delayed"}),
		}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("book_taxi", "sig_B", map[string]any{"time": "10AM"}),
		}},
		{Role: RoleUser, Parts: []Part{
			FunctionResponsePart("book_taxi", map[string]any{"status": "success"}),
		}},
	}

	if err := ValidateFunctionCallSignatures(contents); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestValidateFunctionCallSignaturesSkipWithDummy(t *testing.T) {
	contents := []Content{
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("get_weather", ThoughtSignatureSkip, map[string]any{"city": "Paris"}),
		}},
	}

	if err := ValidateFunctionCallSignatures(contents); err != nil {
		t.Errorf("dummy signature should pass validation, got %v", err)
	}
}

func TestValidateFunctionCallSignaturesNoFunctionCalls(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("hello")}},
		{Role: RoleModel, Parts: []Part{TextPart("hi")}},
	}

	if err := ValidateFunctionCallSignatures(contents); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestMissingSignatureErrorString(t *testing.T) {
	e := &MissingSignatureError{ContentIndex: 1, PartIndex: 0, FunctionName: "get_weather"}
	s := e.Error()
	if s == "" {
		t.Error("error string should not be empty")
	}
}

// ============================================================================
// PreserveModelContent 测试
// ============================================================================

func TestPreserveModelContent(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("get_weather", "sig_A", map[string]any{"city": "Paris"}),
					TextPartWithSignature("thinking done", "sig_B"),
				},
			},
		}},
	}

	content := PreserveModelContent(resp)
	if content == nil {
		t.Fatal("content is nil")
	}
	if content.Role != RoleModel {
		t.Errorf("role = %q, want model", content.Role)
	}
	if len(content.Parts) != 2 {
		t.Fatalf("parts count = %d, want 2", len(content.Parts))
	}
	if content.Parts[0].ThoughtSignature != "sig_A" {
		t.Errorf("parts[0] signature = %q, want sig_A", content.Parts[0].ThoughtSignature)
	}
	if content.Parts[1].ThoughtSignature != "sig_B" {
		t.Errorf("parts[1] signature = %q, want sig_B", content.Parts[1].ThoughtSignature)
	}
}

func TestPreserveModelContentNil(t *testing.T) {
	if c := PreserveModelContent(nil); c != nil {
		t.Error("expected nil for nil response")
	}
	if c := PreserveModelContent(&GenerateContentResponse{}); c != nil {
		t.Error("expected nil for empty candidates")
	}
}

func TestPreserveModelContentImmutable(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{FunctionCallPartWithSignature("fn", "sig", nil)},
			},
		}},
	}

	content := PreserveModelContent(resp)
	// 修改返回的 content 不应影响原始 resp
	content.Parts[0].ThoughtSignature = "changed"

	if resp.Candidates[0].Content.Parts[0].ThoughtSignature != "sig" {
		t.Error("original response was mutated")
	}
}

// ============================================================================
// AttachSignatureToFunctionCall 测试
// ============================================================================

func TestAttachSignatureToFunctionCall(t *testing.T) {
	content := &Content{
		Role: RoleModel,
		Parts: []Part{
			FunctionCallPart("get_weather", map[string]any{"city": "Paris"}), // 无签名
			FunctionCallPart("get_weather", map[string]any{"city": "London"}),
		},
	}

	ok := AttachSignatureToFunctionCall(content, "sig_attached")
	if !ok {
		t.Fatal("expected true")
	}

	if content.Parts[0].ThoughtSignature != "sig_attached" {
		t.Errorf("parts[0] signature = %q, want sig_attached", content.Parts[0].ThoughtSignature)
	}
	// 第二个 FC 不应被修改
	if content.Parts[1].ThoughtSignature != "" {
		t.Errorf("parts[1] signature should be empty, got %q", content.Parts[1].ThoughtSignature)
	}
}

func TestAttachSignatureToFunctionCallNoFC(t *testing.T) {
	content := &Content{
		Role:  RoleModel,
		Parts: []Part{TextPart("hello")},
	}

	if ok := AttachSignatureToFunctionCall(content, "sig"); ok {
		t.Error("expected false for no function calls")
	}
}

func TestAttachSignatureToFunctionCallNil(t *testing.T) {
	if ok := AttachSignatureToFunctionCall(nil, "sig"); ok {
		t.Error("expected false for nil content")
	}
}

// ============================================================================
// AttachSignaturesByPosition 测试
// ============================================================================

func TestAttachSignaturesByPosition(t *testing.T) {
	content := &Content{
		Role: RoleModel,
		Parts: []Part{
			FunctionCallPart("fn1", nil),
			TextPart("text"),
			FunctionCallPart("fn2", nil),
		},
	}

	entries := []ThoughtSignatureEntry{
		{PartIndex: 0, Signature: "sig_0", FunctionName: "fn1"},
		{PartIndex: 2, Signature: "sig_2", FunctionName: "fn2"},
	}

	AttachSignaturesByPosition(content, entries)

	if content.Parts[0].ThoughtSignature != "sig_0" {
		t.Errorf("parts[0] signature = %q", content.Parts[0].ThoughtSignature)
	}
	if content.Parts[2].ThoughtSignature != "sig_2" {
		t.Errorf("parts[2] signature = %q", content.Parts[2].ThoughtSignature)
	}
	if content.Parts[1].ThoughtSignature != "" {
		t.Errorf("parts[1] should be unchanged")
	}
}

// ============================================================================
// BuildFunctionResponseTurn 测试
// ============================================================================

func TestBuildFunctionResponseTurn(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("check_flight", "sig_A", map[string]any{"flight": "AA100"}),
				},
			},
		}},
	}

	functionResponses := []Part{
		FunctionResponsePart("check_flight", map[string]any{"status": "delayed"}),
	}

	contents := BuildFunctionResponseTurn(resp, functionResponses)
	if contents == nil {
		t.Fatal("expected non-nil contents")
	}
	if len(contents) != 2 {
		t.Fatalf("contents count = %d, want 2", len(contents))
	}

	// 第一段：model 消息（保留签名）
	if contents[0].Role != RoleModel {
		t.Errorf("contents[0].Role = %q, want model", contents[0].Role)
	}
	if contents[0].Parts[0].ThoughtSignature != "sig_A" {
		t.Errorf("model content should preserve signature")
	}

	// 第二段：user 消息（函数响应）
	if contents[1].Role != RoleUser {
		t.Errorf("contents[1].Role = %q, want user", contents[1].Role)
	}
	if contents[1].Parts[0].FunctionResponse == nil {
		t.Error("expected FunctionResponse part")
	}
}

func TestBuildFunctionResponseTurnNil(t *testing.T) {
	if c := BuildFunctionResponseTurn(nil, nil); c != nil {
		t.Error("expected nil for nil response")
	}
}

// ============================================================================
// StripThoughtSignatures 测试
// ============================================================================

func TestStripThoughtSignatures(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("query")}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("fn", "sig", nil),
		}},
	}

	StripThoughtSignatures(contents)

	for _, c := range contents {
		for _, p := range c.Parts {
			if p.ThoughtSignature != "" {
				t.Errorf("signature should be stripped: %q", p.ThoughtSignature)
			}
		}
	}
}

// ============================================================================
// StripOldTurnSignatures 测试
// ============================================================================

func TestStripOldTurnSignatures(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{TextPart("query1")}},
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("fn1", "sig_old", nil),
		}},
		{Role: RoleUser, Parts: []Part{TextPart("query2")}}, // 当前轮次开始 (index=2)
		{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithSignature("fn2", "sig_current", nil),
		}},
	}

	StripOldTurnSignatures(contents, 2)

	if contents[1].Parts[0].ThoughtSignature != "" {
		t.Error("old turn signature should be stripped")
	}
	if contents[3].Parts[0].ThoughtSignature != "sig_current" {
		t.Error("current turn signature should be preserved")
	}
}

// ============================================================================
// StreamAccumulator 思考签名测试
// ============================================================================

func TestStreamAccumulatorPreservesFunctionCallSignature(t *testing.T) {
	acc := NewStreamAccumulator()

	// 模拟流式 chunk：带签名的函数调用
	chunk := GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("get_weather", "sig_stream", map[string]any{"city": "Paris"}),
				},
			},
		}},
	}

	_ = acc.OnChunk(chunk)

	result := acc.Result()
	if len(result.Candidates) == 0 {
		t.Fatal("no candidates")
	}

	parts := result.Candidates[0].Content.Parts
	found := false
	for _, p := range parts {
		if p.FunctionCall != nil && p.ThoughtSignature == "sig_stream" {
			found = true
		}
	}
	if !found {
		t.Error("function call signature was not preserved")
	}
}

func TestStreamAccumulatorPreservesTextSignature(t *testing.T) {
	acc := NewStreamAccumulator()

	// 流式 chunk 1: 文本
	_ = acc.OnChunk(GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{TextPart("The weather in Paris is ")},
			},
		}},
	})

	// 流式 chunk 2: 文本 + 签名
	_ = acc.OnChunk(GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{TextPartWithSignature("sunny.", "sig_text")},
			},
		}},
	})

	result := acc.Result()
	parts := result.Candidates[0].Content.Parts

	if parts[0].Text != "The weather in Paris is sunny." {
		t.Errorf("text = %q", parts[0].Text)
	}
	if parts[0].ThoughtSignature != "sig_text" {
		t.Errorf("signature = %q, want sig_text", parts[0].ThoughtSignature)
	}
}

func TestStreamAccumulatorPreservesStandaloneSignature(t *testing.T) {
	acc := NewStreamAccumulator()

	// 流式 chunk: 文本
	_ = acc.OnChunk(GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{TextPart("Hello world")},
			},
		}},
	})

	// 流式 chunk: 仅签名的空部分
	_ = acc.OnChunk(GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{SignaturePart("sig_standalone")},
			},
		}},
	})

	result := acc.Result()
	parts := result.Candidates[0].Content.Parts

	// 签名应附加到聚合文本上
	if parts[0].Text != "Hello world" {
		t.Errorf("text = %q, want 'Hello world'", parts[0].Text)
	}
	if parts[0].ThoughtSignature != "sig_standalone" {
		t.Errorf("signature = %q, want sig_standalone", parts[0].ThoughtSignature)
	}
}

func TestStreamAccumulatorOnlySignature(t *testing.T) {
	acc := NewStreamAccumulator()

	// 只有签名，没有文本
	_ = acc.OnChunk(GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: []Part{SignaturePart("sig_only")},
			},
		}},
	})

	result := acc.Result()
	parts := result.Candidates[0].Content.Parts

	if len(parts) == 0 {
		t.Fatal("no parts in result")
	}
	if parts[0].ThoughtSignature != "sig_only" {
		t.Errorf("signature = %q, want sig_only", parts[0].ThoughtSignature)
	}
}

// ============================================================================
// 端到端：多轮函数调用场景测试
// ============================================================================

func TestMultiTurnFunctionCallingScenario(t *testing.T) {
	// 轮次 1 / 步骤 1: 用户请求
	userMsg := Content{
		Role:  RoleUser,
		Parts: []Part{TextPart("Check flight status for AA100")},
	}

	// 模型响应 1: 函数调用 + 签名 A
	resp1 := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("check_flight", "sig_A", map[string]any{"flight": "AA100"}),
				},
			},
		}},
	}

	// 构建步骤 2 的请求
	step2Contents := BuildFunctionResponseTurn(resp1, []Part{
		FunctionResponsePart("check_flight", map[string]any{"status": "delayed", "departure": "12PM"}),
	})

	// 完整请求
	allContents := append([]Content{userMsg}, step2Contents...)

	// 验证签名
	if err := ValidateFunctionCallSignatures(allContents); err != nil {
		t.Fatalf("step 2 validation failed: %v", err)
	}

	// 模型响应 2: 函数调用 + 签名 B
	resp2 := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("book_taxi", "sig_B", map[string]any{"time": "10AM"}),
				},
			},
		}},
	}

	// 构建步骤 3 的请求（需要保留所有签名）
	step3Contents := BuildFunctionResponseTurn(resp2, []Part{
		FunctionResponsePart("book_taxi", map[string]any{"status": "success"}),
	})

	allContents3 := append(allContents, step3Contents...)

	// 验证所有签名完整
	if err := ValidateFunctionCallSignatures(allContents3); err != nil {
		t.Fatalf("step 3 validation failed: %v", err)
	}

	// 验证签名的值正确
	sigEntries := ExtractThoughtSignatureEntries(&GenerateContentResponse{
		Candidates: []Candidate{{Content: allContents3[1]}}, // model content from step 1
	})
	if len(sigEntries) != 1 || sigEntries[0].Signature != "sig_A" {
		t.Errorf("sig_A not preserved in step 1 model content")
	}

	sigEntries2 := ExtractThoughtSignatureEntries(&GenerateContentResponse{
		Candidates: []Candidate{{Content: allContents3[3]}}, // model content from step 2
	})
	if len(sigEntries2) != 1 || sigEntries2[0].Signature != "sig_B" {
		t.Errorf("sig_B not preserved in step 2 model content")
	}
}

func TestParallelFunctionCallScenario(t *testing.T) {
	// 并行函数调用：签名仅在第一个 FC 上
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role: RoleModel,
				Parts: []Part{
					FunctionCallPartWithSignature("get_weather", "sig_parallel", map[string]any{"city": "Paris"}),
					FunctionCallPart("get_weather", map[string]any{"city": "London"}),
				},
			},
		}},
	}

	// 第一个 FC 有签名
	sig := ExtractFirstFunctionCallSignature(resp)
	if sig != "sig_parallel" {
		t.Errorf("first FC signature = %q, want sig_parallel", sig)
	}

	// 构建响应轮次
	functionResponses := []Part{
		FunctionResponsePart("get_weather", map[string]any{"temp": "15C"}),
		FunctionResponsePart("get_weather", map[string]any{"temp": "12C"}),
	}

	contents := BuildFunctionResponseTurn(resp, functionResponses)

	// 验证：model 内容中的签名被保留
	if contents[0].Parts[0].ThoughtSignature != "sig_parallel" {
		t.Error("parallel FC signature not preserved")
	}

	// 验证整个对话
	allContents := append([]Content{
		{Role: RoleUser, Parts: []Part{TextPart("weather in Paris and London")}},
	}, contents...)

	if err := ValidateFunctionCallSignatures(allContents); err != nil {
		t.Errorf("parallel call validation failed: %v", err)
	}
}
