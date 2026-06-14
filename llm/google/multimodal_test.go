package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ============================================================================
// 图片生成
// ============================================================================

func TestGenerateImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "gemini-2.5-flash-image:generateContent") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		// 验证 generationConfig.responseModalities
		if req.GenerationConfig == nil {
			t.Fatal("expected generation config")
		}
		if len(req.GenerationConfig.ResponseModalities) != 2 {
			t.Fatalf("expected 2 modalities, got %d", len(req.GenerationConfig.ResponseModalities))
		}
		if req.GenerationConfig.ResponseModalities[0] != ModalityText {
			t.Errorf("expected TEXT first, got %s", req.GenerationConfig.ResponseModalities[0])
		}
		if req.GenerationConfig.ResponseModalities[1] != ModalityImage {
			t.Errorf("expected IMAGE second, got %s", req.GenerationConfig.ResponseModalities[1])
		}

		// 验证 responseFormat
		if req.GenerationConfig.ResponseFormat == nil || req.GenerationConfig.ResponseFormat.Image == nil {
			t.Fatal("expected responseFormat.image")
		}
		if req.GenerationConfig.ResponseFormat.Image.AspectRatio != AspectRatio16_9 {
			t.Errorf("expected 16:9, got %s", req.GenerationConfig.ResponseFormat.Image.AspectRatio)
		}
		if req.GenerationConfig.ResponseFormat.Image.ImageSize != ImageSize2K {
			t.Errorf("expected 2K, got %s", req.GenerationConfig.ResponseFormat.Image.ImageSize)
		}

		// 返回带图片的响应
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{
						{Text: "Here is your image:"},
						{InlineData: &Blob{MimeType: "image/png", Data: "iVBORw0KGgo="}},
					},
				},
				FinishReason: FinishReasonStop,
			}},
			ModelVersion: "gemini-2.5-flash-image",
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateImage(context.Background(), ModelGemini25FlashImage, "A cat in a hat", &ImageGenerationOptions{
		AspectRatio: AspectRatio16_9,
		ImageSize:   ImageSize2K,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	images, texts := ExtractImages(resp)
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].MimeType != "image/png" {
		t.Errorf("expected image/png, got %s", images[0].MimeType)
	}
	if len(texts) != 1 || texts[0] != "Here is your image:" {
		t.Errorf("unexpected texts: %v", texts)
	}
}

func TestGenerateImageDefaultModalities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if len(req.GenerationConfig.ResponseModalities) != 2 {
			t.Errorf("expected default [TEXT, IMAGE], got %v", req.GenerationConfig.ResponseModalities)
		}
		if req.GenerationConfig.ResponseFormat != nil {
			t.Error("expected nil responseFormat when no aspect/imageSize")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{InlineData: &Blob{MimeType: "image/png", Data: "abc"}}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateImage(context.Background(), ModelGemini25FlashImage, "hello", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateImageCustomModalities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if len(req.GenerationConfig.ResponseModalities) != 1 {
			t.Fatalf("expected 1 modality, got %d", len(req.GenerationConfig.ResponseModalities))
		}
		if req.GenerationConfig.ResponseModalities[0] != ModalityImage {
			t.Errorf("expected IMAGE only, got %s", req.GenerationConfig.ResponseModalities[0])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{InlineData: &Blob{MimeType: "image/png", Data: "abc"}}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateImage(context.Background(), ModelGemini25FlashImage, "hello", &ImageGenerationOptions{
		ResponseModalities: []ResponseModality{ModalityImage},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================================
// 图片编辑
// ============================================================================

func TestEditImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if len(req.Contents) != 1 {
			t.Fatalf("expected 1 content, got %d", len(req.Contents))
		}
		parts := req.Contents[0].Parts
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts (prompt + image), got %d", len(parts))
		}
		if parts[0].Text != "Make the background blue" {
			t.Errorf("unexpected prompt: %s", parts[0].Text)
		}
		if parts[1].InlineData == nil {
			t.Error("expected inline data for reference image")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{InlineData: &Blob{MimeType: "image/png", Data: "edited"}}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	refImage := ImagePart(MIMEPNG, "originaldata")
	_, err := c.EditImage(context.Background(), ModelGemini25FlashImage, "Make the background blue", []Part{refImage}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditImageNoReference(t *testing.T) {
	c := New(WithAPIKey("test-key"))
	_, err := c.EditImage(context.Background(), ModelGemini25FlashImage, "edit", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "reference image is required") {
		t.Errorf("expected error about reference image, got %v", err)
	}
}

// ============================================================================
// 图片理解（输入图片 + 文本）
// ============================================================================

func TestImageUnderstanding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		if len(req.Contents) != 1 {
			t.Fatalf("expected 1 content, got %d", len(req.Contents))
		}
		parts := req.Contents[0].Parts
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		// 文本应该在图片之后（最佳实践）
		if parts[0].InlineData == nil {
			t.Error("expected inline data part first")
		}
		if parts[0].InlineData.MimeType != MIMEJPEG {
			t.Errorf("expected image/jpeg, got %s", parts[0].InlineData.MimeType)
		}
		if parts[1].Text != "What is in this image?" {
			t.Errorf("unexpected text: %s", parts[1].Text)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "A cat sitting on a chair."}},
				},
				FinishReason: FinishReasonStop,
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				ImagePart(MIMEJPEG, "base64imagedata"),
				TextPart("What is in this image?"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "A cat sitting on a chair." {
		t.Errorf("unexpected text: %s", resp.Candidates[0].Content.Parts[0].Text)
	}
}

// ============================================================================
// 音频理解
// ============================================================================

func TestAudioUnderstanding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		parts := req.Contents[0].Parts
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		if parts[0].Text != "Transcribe this audio" {
			t.Errorf("unexpected text: %s", parts[0].Text)
		}
		if parts[1].InlineData == nil || parts[1].InlineData.MimeType != MIMEMP3 {
			t.Errorf("expected audio/mp3 inline data")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "Hello world."}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				TextPart("Transcribe this audio"),
				AudioPart(MIMEMP3, "base64audiodata"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "Hello world." {
		t.Errorf("unexpected text: %s", resp.Candidates[0].Content.Parts[0].Text)
	}
}

// ============================================================================
// 视频理解
// ============================================================================

func TestVideoUnderstanding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		parts := req.Contents[0].Parts
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		if parts[0].InlineData == nil || parts[0].InlineData.MimeType != MIMEVideoMP4 {
			t.Errorf("expected video/mp4 inline data")
		}
		if parts[0].VideoMetadata == nil {
			t.Error("expected video metadata")
		}
		if parts[0].VideoMetadata.FPS != 1.0 {
			t.Errorf("expected fps=1.0, got %f", parts[0].VideoMetadata.FPS)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "The video shows a sunset."}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				VideoPartWithMetadata(MIMEVideoMP4, "base64videodata", 1.0),
				TextPart("Describe this video"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "The video shows a sunset." {
		t.Errorf("unexpected text: %s", resp.Candidates[0].Content.Parts[0].Text)
	}
}

func TestVideoFilePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		parts := req.Contents[0].Parts
		if parts[0].FileData == nil {
			t.Fatal("expected fileData")
		}
		if parts[0].FileData.FileURI != "https://example.com/video.mp4" {
			t.Errorf("unexpected fileURI: %s", parts[0].FileData.FileURI)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "OK"}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				VideoFilePartWithMetadata(MIMEVideoMP4, "https://example.com/video.mp4", 0.5),
				TextPart("Describe"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================================
// YouTube Part
// ============================================================================

func TestYouTubePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		parts := req.Contents[0].Parts
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		if parts[0].FileData == nil || parts[0].FileData.FileURI != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
			t.Errorf("unexpected YouTube URL: %+v", parts[0].FileData)
		}
		if parts[0].VideoMetadata != nil && parts[0].VideoMetadata.FPS != 0.5 {
			t.Errorf("expected fps=0.5, got %f", parts[0].VideoMetadata.FPS)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "Summary of video."}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				YouTubePartWithMetadata("https://www.youtube.com/watch?v=dQw4w9WgXcQ", 0.5),
				TextPart("Summarize this video"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================================
// 多图片输入
// ============================================================================

func TestMultipleImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)

		parts := req.Contents[0].Parts
		// 1 text + 2 images
		if len(parts) != 3 {
			t.Fatalf("expected 3 parts, got %d", len(parts))
		}
		if parts[0].Text == "" {
			t.Error("expected text first")
		}
		if parts[1].InlineData == nil || parts[2].InlineData == nil {
			t.Error("expected 2 inline data parts")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GenerateContentResponse{
			Candidates: []Candidate{{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{{Text: "Differences: ..."}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", GenerateContentRequest{
		Contents: []Content{{
			Role: RoleUser,
			Parts: []Part{
				TextPart("What's different between these images?"),
				ImagePart(MIMEJPEG, "img1base64"),
				ImagePart(MIMEPNG, "img2base64"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================================
// 多模态 Part 构造器
// ============================================================================

func TestMultimodalPartConstructors(t *testing.T) {
	// ImagePart
	p := ImagePart(MIMEPNG, "abc123")
	if p.InlineData == nil || p.InlineData.MimeType != MIMEPNG || p.InlineData.Data != "abc123" {
		t.Errorf("ImagePart unexpected: %+v", p)
	}

	// ImageFilePart
	p = ImageFilePart(MIMEJPEG, "https://example.com/img.jpg")
	if p.FileData == nil || p.FileData.MimeType != MIMEJPEG || p.FileData.FileURI != "https://example.com/img.jpg" {
		t.Errorf("ImageFilePart unexpected: %+v", p)
	}

	// AudioPart
	p = AudioPart(MIMEWAV, "audio")
	if p.InlineData == nil || p.InlineData.MimeType != MIMEWAV {
		t.Errorf("AudioPart unexpected: %+v", p)
	}

	// AudioFilePart
	p = AudioFilePart(MIMEMP3, "uri")
	if p.FileData == nil || p.FileData.MimeType != MIMEMP3 {
		t.Errorf("AudioFilePart unexpected: %+v", p)
	}

	// VideoPart
	p = VideoPart(MIMEVideoMP4, "video")
	if p.InlineData == nil || p.InlineData.MimeType != MIMEVideoMP4 {
		t.Errorf("VideoPart unexpected: %+v", p)
	}

	// VideoFilePart
	p = VideoFilePart(MIMEVideoAVI, "uri")
	if p.FileData == nil || p.FileData.FileURI != "uri" {
		t.Errorf("VideoFilePart unexpected: %+v", p)
	}

	// VideoPartWithMetadata
	p = VideoPartWithMetadata(MIMEVideoMOV, "data", 2.0)
	if p.InlineData == nil || p.VideoMetadata == nil || p.VideoMetadata.FPS != 2.0 {
		t.Errorf("VideoPartWithMetadata unexpected: %+v", p)
	}

	// VideoFilePartWithMetadata
	p = VideoFilePartWithMetadata(MIMEVideoWebM, "uri", 1.5)
	if p.FileData == nil || p.VideoMetadata == nil || p.VideoMetadata.FPS != 1.5 {
		t.Errorf("VideoFilePartWithMetadata unexpected: %+v", p)
	}

	// YouTubePart
	p = YouTubePart("https://www.youtube.com/watch?v=abc")
	if p.FileData == nil || p.FileData.FileURI != "https://www.youtube.com/watch?v=abc" {
		t.Errorf("YouTubePart unexpected: %+v", p)
	}

	// YouTubePartWithMetadata
	p = YouTubePartWithMetadata("https://youtu.be/abc", 1.0)
	if p.FileData == nil || p.VideoMetadata == nil || p.VideoMetadata.FPS != 1.0 {
		t.Errorf("YouTubePartWithMetadata unexpected: %+v", p)
	}
}

// ============================================================================
// 提取工具
// ============================================================================

func TestExtractImages(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Parts: []Part{
					{Text: "Here is image 1:"},
					{InlineData: &Blob{MimeType: "image/png", Data: "png1"}},
					{Text: "And image 2:"},
					{InlineData: &Blob{MimeType: "image/jpeg", Data: "jpg1"}},
					{Text: "Thinking...", Thought: true},
				},
			},
		}},
	}

	images, texts := ExtractImages(resp)
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}
	if images[0].Data != "png1" || images[1].Data != "jpg1" {
		t.Errorf("unexpected image data: %v", images)
	}
	if len(texts) != 2 {
		t.Fatalf("expected 2 texts, got %d (thoughts should be excluded)", len(texts))
	}
}

func TestExtractImagesNil(t *testing.T) {
	images, texts := ExtractImages(nil)
	if images != nil || texts != nil {
		t.Error("expected nil for nil response")
	}
}

func TestExtractMedia(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Parts: []Part{
					{Text: "hello"},
					{InlineData: &Blob{MimeType: "image/png", Data: "img"}},
					{InlineData: &Blob{MimeType: "audio/mp3", Data: "audio"}},
					{InlineData: &Blob{MimeType: "video/mp4", Data: "video"}},
				},
			},
		}},
	}

	media := ExtractMedia(resp)
	if len(media) != 3 {
		t.Fatalf("expected 3 media items, got %d", len(media))
	}
}

func TestExtractMediaNil(t *testing.T) {
	if ExtractMedia(nil) != nil {
		t.Error("expected nil for nil response")
	}
}

func TestExtractThoughtSignatures(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Parts: []Part{
					{Text: "Thinking...", Thought: true, ThoughtSignature: "sig1"},
					{InlineData: &Blob{MimeType: "image/png", Data: "img"}, ThoughtSignature: "sig2"},
					{Text: "Answer"},
				},
			},
		}},
	}

	sigs := ExtractThoughtSignatures(resp)
	if len(sigs) != 2 {
		t.Fatalf("expected 2 signature parts, got %d", len(sigs))
	}
}

func TestExtractThoughtSignaturesNil(t *testing.T) {
	if ExtractThoughtSignatures(nil) != nil {
		t.Error("expected nil for nil response")
	}
}

// ============================================================================
// Base64 辅助
// ============================================================================

func TestEncodeDecodeBase64(t *testing.T) {
	original := []byte("Hello, World!")

	encoded := EncodeBase64(original)
	if encoded != base64.StdEncoding.EncodeToString(original) {
		t.Error("encode mismatch")
	}

	decoded, err := DecodeBase64(encoded)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if string(decoded) != "Hello, World!" {
		t.Errorf("unexpected decoded: %s", decoded)
	}
}

func TestSaveImageToFile(t *testing.T) {
	tmpFile := t.TempDir() + "/test.png"
	blob := Blob{MimeType: "image/png", Data: EncodeBase64([]byte("PNGBYTES"))}

	if err := SaveImageToFile(blob, tmpFile); err != nil {
		t.Fatalf("save error: %v", err)
	}

	// 验证
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if string(data) != "PNGBYTES" {
		t.Errorf("unexpected file content: %s", data)
	}
}

// ============================================================================
// 流式图片生成
// ============================================================================

func TestStreamImageGeneration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("expected alt=sse")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Generating..."}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"chunk1"}}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"chunk2"}}]}}],"finishReason":"STOP"}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	acc := NewStreamAccumulator()
	err := c.StreamGenerateContent(context.Background(), ModelGemini25FlashImage, GenerateContentRequest{
		Contents: []Content{{
			Role:  RoleUser,
			Parts: []Part{TextPart("Draw a cat")},
		}},
		GenerationConfig: &GenerationConfig{
			ResponseModalities: []ResponseModality{ModalityText, ModalityImage},
		},
	}, acc.OnChunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := acc.Result()
	parts := result.Candidates[0].Content.Parts

	// 应该有 1 个文本 part + 2 个 image part
	var textCount, imageCount int
	for _, p := range parts {
		if p.Text != "" {
			textCount++
		}
		if p.InlineData != nil {
			imageCount++
		}
	}
	if textCount != 1 {
		t.Errorf("expected 1 text part, got %d", textCount)
	}
	if imageCount != 2 {
		t.Errorf("expected 2 image parts, got %d", imageCount)
	}
}

// ============================================================================
// File API
// ============================================================================

func TestUploadFile(t *testing.T) {
	var uploadURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 第一个请求：start upload
		if r.Header.Get("X-Goog-Upload-Command") == "start" {
			w.Header().Set("X-Goog-Upload-Url", uploadURL+"/upload/finalize")
			w.WriteHeader(http.StatusOK)
			return
		}

		// 第二个请求：upload + finalize
		if r.Header.Get("X-Goog-Upload-Command") == "upload, finalize" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"file": map[string]any{
					"name":        "files/test123",
					"displayName": "test.jpg",
					"mimeType":    "image/jpeg",
					"uri":         "https://generativelanguage.googleapis.com/v1beta/files/test123",
					"sizeBytes":   5,
				},
			})
			return
		}

		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	uploadURL = srv.URL

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	file, err := c.UploadFile(context.Background(), strings.NewReader("hello"), 5, UploadFileOptions{
		DisplayName: "test.jpg",
		MimeType:    MIMEJPEG,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if file.Name != "files/test123" {
		t.Errorf("unexpected file name: %s", file.Name)
	}
	if file.MimeType != MIMEJPEG {
		t.Errorf("unexpected mime type: %s", file.MimeType)
	}
}

func TestListFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageSize") != "10" {
			t.Errorf("expected pageSize=10, got %s", r.URL.Query().Get("pageSize"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListFilesResponse{
			Files: []File{
				{Name: "files/abc", DisplayName: "img1"},
				{Name: "files/def", DisplayName: "audio1"},
			},
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := c.ListFiles(context.Background(), &ListFilesOptions{PageSize: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(resp.Files))
	}
}

func TestGetFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "files/abc") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(File{
			Name:     "files/abc",
			MimeType: "image/png",
			URI:      "https://generativelanguage.googleapis.com/v1beta/files/abc",
		})
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	file, err := c.GetFile(context.Background(), "files/abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if file.Name != "files/abc" {
		t.Errorf("unexpected name: %s", file.Name)
	}
}

func TestDeleteFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	if err := c.DeleteFile(context.Background(), "files/abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================================
// GenerationConfig 多模态字段
// ============================================================================

func TestGenerationConfigMultimodalSerialization(t *testing.T) {
	cfg := GenerationConfig{
		ResponseModalities: []ResponseModality{ModalityText, ModalityImage},
		ResponseFormat: &ResponseFormat{
			Image: &ImageResponseFormat{
				AspectRatio: AspectRatio1_1,
				ImageSize:   ImageSize2K,
			},
		},
		MediaResolution: MediaResolutionHigh,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"responseModalities":["TEXT","IMAGE"]`) {
		t.Errorf("missing responseModalities in JSON: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"aspectRatio":"1:1"`) {
		t.Errorf("missing aspectRatio in JSON: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"imageSize":"2K"`) {
		t.Errorf("missing imageSize in JSON: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"mediaResolution":"MEDIA_RESOLUTION_HIGH"`) {
		t.Errorf("missing mediaResolution in JSON: %s", jsonStr)
	}
}

func TestPartVideoMetadataSerialization(t *testing.T) {
	p := VideoPartWithMetadata(MIMEVideoMP4, "data", 0.5)

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"videoMetadata"`) {
		t.Errorf("missing videoMetadata in JSON: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"fps":0.5`) {
		t.Errorf("missing fps in JSON: %s", jsonStr)
	}
}
