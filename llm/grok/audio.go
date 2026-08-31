package grok

import (
	"bytes"
	"context"
	"strconv"

	httputil "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// Text to Speech (TTS)
// ============================================================================

// TTSVoice IDs。
const (
	VoiceEve = "eve" // 默认，活泼
	VoiceAra = "ara" // 温暖友好
	VoiceRex = "rex" // 自信清晰
	VoiceSal = "sal" // 平衡
	VoiceLeo = "leo" // 威严
)

// TTS 语音转文本合成语音，返回原始音频字节。
//
// codec 默认 mp3（24kHz/128kbps）。
// language 为 BCP-47 语言代码（如 "en", "zh"）或 "auto"。
func (c *Client) TTS(ctx context.Context, text, voiceID, language string, opts ...TTSOption) ([]byte, string, error) {
	req := TTSRequest{
		Text:     text,
		VoiceID:  voiceID,
		Language: language,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoTTS(ctx, req)
}

// TTSOption TTS 请求选项。
type TTSOption func(*TTSRequest)

// WithTTSSpeed 设置语速倍率（0.7-1.5）。
func WithTTSSpeed(speed float64) TTSOption {
	return func(r *TTSRequest) { r.Speed = &speed }
}

// WithTTSOutputFormat 设置输出格式。
func WithTTSOutputFormat(codec string, sampleRate, bitRate int) TTSOption {
	return func(r *TTSRequest) {
		r.OutputFormat = &TTSOutputFormat{
			Codec:      codec,
			SampleRate: sampleRate,
			BitRate:    bitRate,
		}
	}
}

// WithTTSOptimizeStreamingLatency 设置流式延迟优化级别（0, 1, 2）。
func WithTTSOptimizeStreamingLatency(level int) TTSOption {
	return func(r *TTSRequest) { r.OptimizeStreamingLatency = &level }
}

// WithTTSTextNormalization 启用文本规范化。
func WithTTSTextNormalization(enabled bool) TTSOption {
	return func(r *TTSRequest) { r.TextNormalization = &enabled }
}

// DoTTS 发送完整的 TTS 请求，返回音频字节和 Content-Type。
func (c *Client) DoTTS(ctx context.Context, req TTSRequest) ([]byte, string, error) {
	if req.Text == "" {
		return nil, "", grokError("grok: text is required for TTS")
	}
	if req.Language == "" {
		return nil, "", grokError("grok: language is required for TTS")
	}

	resp, err := c.newRequest("POST", "/v1/tts").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, "", parseAPIError(resp, err)
	}

	contentType := resp.Headers.Get("Content-Type")
	return resp.Body, contentType, nil
}

// ListVoices 列出可用的 TTS 语音。
func (c *Client) ListVoices(ctx context.Context) (*ListVoicesResponse, error) {
	resp, err := c.newRequest("GET", "/v1/tts/voices").
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ListVoicesResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Speech to Text (STT)
// ============================================================================

// STTOption STT 请求选项。
type STTOption func(*STTRequest)

// WithSTTLanguage 设置语言代码。
func WithSTTLanguage(lang string) STTOption {
	return func(r *STTRequest) { r.Language = lang }
}

// WithSTTFormat 启用文本格式化。
func WithSTTFormat(format bool) STTOption {
	return func(r *STTRequest) { r.Format = format }
}

// WithSTTMultichannel 启用多通道转录。
func WithSTTMultichannel(multi bool) STTOption {
	return func(r *STTRequest) { r.Multichannel = multi }
}

// WithSTTChannels 设置通道数（2-8）。
func WithSTTChannels(n int) STTOption {
	return func(r *STTRequest) { r.Channels = n }
}

// WithSTTDiarize 启用说话人分离。
func WithSTTDiarize(diarize bool) STTOption {
	return func(r *STTRequest) { r.Diarize = diarize }
}

// WithSTTKeyTerms 设置关键词列表。
func WithSTTKeyTerms(terms ...string) STTOption {
	return func(r *STTRequest) { r.KeyTerms = terms }
}

// WithSTTFillerWords 保留填充词。
func WithSTTFillerWords(fillers bool) STTOption {
	return func(r *STTRequest) { r.FillerWords = fillers }
}

// WithSTTRawFormat 设置原始音频格式和采样率。
func WithSTTRawFormat(format string, sampleRate int) STTOption {
	return func(r *STTRequest) {
		r.AudioFormat = format
		r.SampleRate = sampleRate
	}
}

// SpeechToText 从文件转录语音。
func (c *Client) SpeechToText(ctx context.Context, params STTRequest, opts ...STTOption) (*STTResponse, error) {
	for _, opt := range opts {
		opt(&params)
	}
	return c.DoSpeechToText(ctx, params)
}

// SpeechToTextFromBytes 从字节转录语音。
func (c *Client) SpeechToTextFromBytes(ctx context.Context, filename string, data []byte, opts ...STTOption) (*STTResponse, error) {
	params := STTRequest{
		File:     data,
		Filename: filename,
	}
	for _, opt := range opts {
		opt(&params)
	}
	return c.DoSpeechToText(ctx, params)
}

// SpeechToTextFromURL 从远程 URL 转录语音。
func (c *Client) SpeechToTextFromURL(ctx context.Context, url string, opts ...STTOption) (*STTResponse, error) {
	params := STTRequest{URL: url}
	for _, opt := range opts {
		opt(&params)
	}
	return c.DoSpeechToText(ctx, params)
}

// DoSpeechToText 发送完整的 STT 请求。
//
// 注意：file 字段必须是 multipart 表单的最后一个字段。
func (c *Client) DoSpeechToText(ctx context.Context, params STTRequest) (*STTResponse, error) {
	if len(params.File) == 0 && params.URL == "" {
		return nil, grokError("grok: either file or url is required for STT")
	}

	form := httputil.NewMultipartForm()

	// 先添加所有非文件字段
	if params.URL != "" {
		form.AddField("url", params.URL)
	}
	if params.AudioFormat != "" {
		form.AddField("audio_format", params.AudioFormat)
	}
	if params.SampleRate > 0 {
		form.AddField("sample_rate", intStr(params.SampleRate))
	}
	if params.Language != "" {
		form.AddField("language", params.Language)
	}
	if params.Format {
		form.AddField("format", "true")
	}
	if params.Multichannel {
		form.AddField("multichannel", "true")
	}
	if params.Channels > 0 {
		form.AddField("channels", intStr(params.Channels))
	}
	if params.Diarize {
		form.AddField("diarize", "true")
	}
	for _, term := range params.KeyTerms {
		form.AddField("keyterm", term)
	}
	if params.FillerWords {
		form.AddField("filler_words", "true")
	}

	// file 字段必须是最后一个
	if len(params.File) > 0 {
		form.AddFile("file", params.Filename, bytes.NewReader(params.File))
	}

	resp, err := c.newRequest("POST", "/v1/stt").
		SetContext(ctx).
		SetMultipart(form).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result STTResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// intStr 将整数转为字符串。
func intStr(n int) string {
	return strconv.Itoa(n)
}
