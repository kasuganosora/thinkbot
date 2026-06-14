package openai

import (
	"context"
	"strconv"
	"strings"

	httputil "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// Text to Speech (TTS)
// ============================================================================

// SpeechOption TTS 请求选项。
type SpeechOption func(*SpeechRequest)

// WithSpeechFormat 设置音频格式（mp3/opus/aac/flac/wav/pcm）。
func WithSpeechFormat(format string) SpeechOption {
	return func(r *SpeechRequest) { r.ResponseFormat = format }
}

// WithSpeechSpeed 设置语速（0.25-4.0）。
func WithSpeechSpeed(speed float64) SpeechOption {
	return func(r *SpeechRequest) { r.Speed = &speed }
}

// WithSpeechInstructions 设置语音风格指令。
func WithSpeechInstructions(instructions string) SpeechOption {
	return func(r *SpeechRequest) { r.Instructions = instructions }
}

// WithSpeechStreamFormat 设置流式格式（sse/audio）。
func WithSpeechStreamFormat(format string) SpeechOption {
	return func(r *SpeechRequest) { r.StreamFormat = format }
}

// Speech 合成语音，返回原始音频字节和 Content-Type。
//
// model 常用 ModelGPT4oMiniTTS / ModelTTS1 / ModelTTS1HD。
// voice 常用 VoiceAlloy / VoiceNova / VoiceEcho 等。
func (c *Client) Speech(ctx context.Context, model, voice, input string, opts ...SpeechOption) ([]byte, string, error) {
	req := SpeechRequest{
		Model: model,
		Voice: voice,
		Input: input,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoSpeech(ctx, req)
}

// DoSpeech 发送完整的 TTS 请求，返回音频字节和 Content-Type。
func (c *Client) DoSpeech(ctx context.Context, req SpeechRequest) ([]byte, string, error) {
	if req.Input == "" {
		return nil, "", openaiError("openai: input is required for speech")
	}
	if req.Model == "" {
		return nil, "", openaiError("openai: model is required for speech")
	}
	if req.Voice == "" {
		return nil, "", openaiError("openai: voice is required for speech")
	}

	// 增大响应体限制（音频可能较大）
	r := c.newRequest("POST", "/v1/audio/speech").
		SetContext(ctx).
		SetJSONBody(req).
		SetHeader("Accept", "audio/mpeg")

	resp, err := r.Do()
	if err != nil {
		return nil, "", parseAPIError(resp, err)
	}

	contentType := resp.Headers.Get("Content-Type")
	return resp.Body, contentType, nil
}

// ============================================================================
// Translation
// ============================================================================

// TranslationOption 翻译请求选项。
type TranslationOption func(*TranslationRequest)

// WithTranslationPrompt 设置引导文本。
func WithTranslationPrompt(prompt string) TranslationOption {
	return func(r *TranslationRequest) { r.Prompt = prompt }
}

// WithTranslationFormat 设置响应格式（json/text/srt/verbose_json/vtt）。
func WithTranslationFormat(format string) TranslationOption {
	return func(r *TranslationRequest) { r.ResponseFormat = format }
}

// WithTranslationTemperature 设置采样温度。
func WithTranslationTemperature(temp float64) TranslationOption {
	return func(r *TranslationRequest) { r.Temperature = &temp }
}

// Translate 从文件翻译音频为英文。
func (c *Client) Translate(ctx context.Context, params TranslationRequest, opts ...TranslationOption) (*TranslationResponse, error) {
	for _, opt := range opts {
		opt(&params)
	}
	return c.DoTranslate(ctx, params)
}

// TranslateFromBytes 从字节翻译音频。
func (c *Client) TranslateFromBytes(ctx context.Context, filename string, data []byte, model string, opts ...TranslationOption) (*TranslationResponse, error) {
	params := TranslationRequest{
		File:     data,
		Filename: filename,
		Model:    model,
	}
	for _, opt := range opts {
		opt(&params)
	}
	return c.DoTranslate(ctx, params)
}

// DoTranslate 发送完整的翻译请求。
//
// 响应格式为 json/text/srt/vtt 时返回文本字段，
// verbose_json 时包含 duration/segments。
func (c *Client) DoTranslate(ctx context.Context, params TranslationRequest) (*TranslationResponse, error) {
	if len(params.File) == 0 {
		return nil, openaiError("openai: file is required for translation")
	}
	if params.Model == "" {
		return nil, openaiError("openai: model is required for translation")
	}

	form := httputil.NewMultipartForm()

	form.AddField("model", params.Model)

	if params.Prompt != "" {
		form.AddField("prompt", params.Prompt)
	}

	responseFormat := params.ResponseFormat
	if responseFormat == "" {
		responseFormat = "json"
	}
	form.AddField("response_format", responseFormat)

	if params.Temperature != nil {
		form.AddField("temperature", strconv.FormatFloat(*params.Temperature, 'f', -1, 64))
	}

	// file 字段必须是最后一个
	mimeType := guessAudioMIME(params.Filename)
	if mimeType != "" {
		form.AddFileWithMIME("file", params.Filename, mimeType, strings.NewReader(string(params.File)))
	} else {
		form.AddFile("file", params.Filename, strings.NewReader(string(params.File)))
	}

	resp, err := c.newRequest("POST", "/v1/audio/translations").
		SetContext(ctx).
		SetMultipart(form).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	// text/srt/vtt 格式直接返回文本
	if responseFormat == "text" || responseFormat == "srt" || responseFormat == "vtt" {
		return &TranslationResponse{
			Text: string(resp.Body),
		}, nil
	}

	// json/verbose_json
	var result TranslationResponse
	if err := resp.JSON(&result); err != nil {
		// 某些情况下 json 格式只有 {"text": "..."} 而没有其他字段
		return &TranslationResponse{
			Text: string(resp.Body),
		}, nil
	}
	return &result, nil
}

// ============================================================================
// Voice Creation
// ============================================================================

// CreateVoice 创建一个自定义语音。
//
// audioSample 为音频文件内容，consent 为同意录音 ID（如 "cons_1234"），name 为语音名称。
func (c *Client) CreateVoice(ctx context.Context, params VoiceCreateRequest) (*Voice, error) {
	if len(params.AudioSample) == 0 {
		return nil, openaiError("openai: audio_sample is required for voice creation")
	}
	if params.Name == "" {
		return nil, openaiError("openai: name is required for voice creation")
	}
	if params.Consent == "" {
		return nil, openaiError("openai: consent is required for voice creation")
	}

	form := httputil.NewMultipartForm()

	form.AddField("name", params.Name)
	form.AddField("consent", params.Consent)

	mimeType := params.ContentType
	if mimeType == "" {
		mimeType = guessAudioMIME(params.Filename)
	}
	if mimeType != "" {
		form.AddFileWithMIME("audio_sample", params.Filename, mimeType, strings.NewReader(string(params.AudioSample)))
	} else {
		form.AddFile("audio_sample", params.Filename, strings.NewReader(string(params.AudioSample)))
	}

	resp, err := c.newRequest("POST", "/v1/audio/voices").
		SetContext(ctx).
		SetMultipart(form).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result Voice
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// 内部工具
// ============================================================================

// openaiError 创建一个简单的 error。
func openaiError(msg string) error {
	return &APIError{Message: msg}
}

// guessAudioMIME 根据文件扩展名猜测 MIME 类型。
func guessAudioMIME(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(lower, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(lower, ".aac"):
		return "audio/aac"
	case strings.HasSuffix(lower, ".flac"):
		return "audio/flac"
	case strings.HasSuffix(lower, ".webm"):
		return "audio/webm"
	case strings.HasSuffix(lower, ".m4a"):
		return "audio/mp4"
	case strings.HasSuffix(lower, ".mp4"):
		return "audio/mp4"
	default:
		return ""
	}
}
