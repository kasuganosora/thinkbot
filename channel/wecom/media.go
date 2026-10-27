package wecom

// 入站媒体（图片 / 语音 / 文件 / 视频）的下载与解密。
//
// 企微回调里媒体只有两个字段：`url` 和 `aeskey`，**没有文件名、没有大小、
// 没有 MIME**（实测，见 docs/wecom-channel-design.md §9.2）。所以：
//   - 必须自己 GET url 拿到**密文**，再用 aeskey 解密
//   - 文件名只能自己造（用 msgid + 序号 + 嗅探出的扩展名）
//   - MIME 只能靠 `net/http.DetectContentType` 嗅探前 512 字节

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

const (
	// wecomMediaTimeout 单个媒体的下载超时。
	wecomMediaTimeout = 30 * time.Second
	// wecomMediaMaxBytes 单个媒体的大小上限（下载后、解密前的密文长度）。
	// 企微侧对临时素材有 20MB 限制，这里对齐即可。
	wecomMediaMaxBytes = 20 << 20
	// wecomInlineTextMax 文本内容内联进正文的上限（与 Telegram 的 maxInlineTextBytes 同思路）。
	wecomInlineTextMax = 64 << 10
	// wecomMaxMediaPerMsg 单条消息最多处理几个媒体（mixed 可能带一堆图，别被刷爆）。
	wecomMaxMediaPerMsg = 4
	// wecomMediaAttempts 下载+解密的尝试次数。CDN 偶发回半截 body，重下一次能救回，
	// 但 URL 一次性，重试只在**同一条回调内**才有意义。
	wecomMediaAttempts = 2
	// wecomMediaRetryDelay 两次尝试之间的间隔。
	wecomMediaRetryDelay = 300 * time.Millisecond
	// wecomMediaDumpEnv 设为 "1" 时，解不开的密文会落盘到临时目录（见 dumpFailedMedia）。
	wecomMediaDumpEnv = "WECOM_MEDIA_DUMP"
	// wecomPadBlock 企微 PKCS#7 的填充粒度 —— **是 32，不是 AES 的块长 16**。
	//
	// 与企微官方加解密样例（PKCS7Encoder，block_size=32）一致：明文填充到 **32 字节边界**，
	// 填充字节数的取值是 1..32（不是标准 PKCS#7 的 1..16）。AES-CBC 仍按 16 字节块运算，
	// 只有「去填充」这一步按 32 校验。
	//
	// ⚠️ 这个差异**一半概率才暴露**：设明文长度 L，
	//   - L%32 >= 16：填充值 n = 32-(L%32) = 16-(L%16)，与标准 PKCS#7 完全重合 → 一切正常
	//   - L%32 <  16：填充值 n = 标准值 + 16，落在 17..32 → 按 16 校验直接报
	//     `invalid pkcs7 padding size N`，媒体整条丢弃
	// 于是表现为「有些文件收得到、有些收不到」，而且失败的看起来毫无规律。
	// 2026-10-20 线上实测：PNG 1975025 B（L%32=17，n=15）与文件 722420 B（L%32=20，n=12）
	// 都恰好落在重合区所以成功；第三个文件报 `padding size 32`（L%32=0，n=32）才暴露。
	wecomPadBlock = 32
)

// mediaItem 是回调体里解析出的一个媒体载荷。
type mediaItem struct {
	Type   string // image / voice / file / video
	URL    string
	AESKey string
}

// fetchMedia 下载并解密媒体，返回附件列表与要并入正文的占位/内联文本。
//
// ⚠️ 单个媒体失败**不**中断整条消息：回退成一行占位文本，保证消息正常入站。
// 媒体是增值信息，正文没了才叫坏。
func (c *WeComChannel) fetchMedia(ctx context.Context, msgID string, items []mediaItem) ([]core.Attachment, []string) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > wecomMaxMediaPerMsg {
		traceid.L(ctx).Warnw("wecom: too many media items in one message, dropping the rest",
			"channel", c.name, "msgid", msgID, "count", len(items), "cap", wecomMaxMediaPerMsg)
		items = items[:wecomMaxMediaPerMsg]
	}

	var (
		atts  []core.Attachment
		notes []string
	)
	for i, it := range items {
		if it.URL == "" || it.AESKey == "" {
			// 没有密钥就没法解 —— 明确告诉模型「有媒体但拿不到」，
			// 别让它以为用户只是说了句空话。
			notes = append(notes, fmt.Sprintf("[%s：缺少解密密钥，无法读取内容]", mediaLabel(it.Type)))
			continue
		}
		data, err := c.downloadAndDecrypt(ctx, msgID, it)
		if err != nil {
			traceid.L(ctx).Warnw("wecom: media download/decrypt failed",
				"channel", c.name, "msgid", msgID, "type", it.Type, "err", err)
			notes = append(notes, fmt.Sprintf("[%s：下载或解密失败]", mediaLabel(it.Type)))
			continue
		}

		mime := nethttp.DetectContentType(data)
		name := mediaFilename(it.Type, mime, msgID, i)
		att := core.Attachment{
			Type:     attachmentType(it.Type, mime),
			MimeType: mime,
			Data:     data,
			Filename: name,
		}
		atts = append(atts, att)

		// 小文本文件直接内联：让 LLM 真的读到内容 —— 只挂附件的话，
		// 模型看到的是 "[文件]"，会像之前那样回答"没收到文件"。
		if isTextualMIME(mime) && len(data) <= wecomInlineTextMax && utf8.Valid(data) {
			notes = append(notes, formatInlineFile(name, mime, string(data)))
			traceid.L(ctx).Infow("wecom: text media inlined",
				"channel", c.name, "msgid", msgID, "name", name, "mime", mime, "bytes", len(data))
			continue
		}

		// 图片：走附件（多模态链路直送模型），**不**落工作空间 ——
		// 否则每收一张图就在 inbound/ 里堆一份，而模型其实看的是附件不是路径。
		if core.IsImageType(att.Type) {
			notes = append(notes, fmt.Sprintf("[%s：%s，%s]", mediaLabel(it.Type), name, humanBytes(len(data))))
			traceid.L(ctx).Infow("wecom: image received",
				"channel", c.name, "msgid", msgID, "name", name, "mime", mime, "bytes", len(data))
			continue
		}

		// 大文件/二进制：优先落工作空间并给出路径，模型用 read_file 自己读。
		// ⚠️ 只挂附件是不够的：705KB 的文本文件挂上去，模型照样看不见内容
		// （2026-10-20 实测「文件渠道测试」就是这么失败的）。
		if c.fileSink != nil {
			if rel, werr := c.fileSink(ctx, c.botID, name, data); werr == nil {
				notes = append(notes, formatSinkFile(name, mime, rel, len(data)))
				traceid.L(ctx).Infow("wecom: media saved to workspace",
					"channel", c.name, "msgid", msgID, "name", name, "rel", rel, "bytes", len(data))
				continue
			} else {
				traceid.L(ctx).Warnw("wecom: save media to workspace failed, falling back to placeholder",
					"channel", c.name, "msgid", msgID, "name", name, "err", werr)
			}
		}
		notes = append(notes, fmt.Sprintf("[%s：%s，%s]", mediaLabel(it.Type), name, humanBytes(len(data))))
		traceid.L(ctx).Infow("wecom: binary media received",
			"channel", c.name, "msgid", msgID, "name", name, "mime", mime, "bytes", len(data))
	}
	return atts, notes
}

// downloadAndDecrypt 下载密文并用 aeskey 解密。
//
// ⚠️ 媒体 URL 是**一次性/短时效**的（实测几分钟内失效），解密失败后无法回放。
// 所以这里把「失败现场的证据」一次性留够：Content-Type、长度、头尾 hex、以及
// （WECOM_MEDIA_DUMP=1 时）密文落盘，避免只能对着一行 "invalid pkcs7 padding" 猜。
func (c *WeComChannel) downloadAndDecrypt(ctx context.Context, msgID string, it mediaItem) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < wecomMediaAttempts; attempt++ {
		ct, ctType, err := c.downloadMedia(ctx, it)
		if err != nil {
			lastErr = err
			// 最后一次失败后不必再睡：白等 300ms 只是把入站再拖慢一点。
			if attempt+1 < wecomMediaAttempts {
				if serr := sleepCtx(ctx, wecomMediaRetryDelay); serr != nil {
					return nil, lastErr
				}
			}
			continue
		}
		// 拿到的东西根本不是密文（错误页 / 未加密直出）→ 重试没有意义，直接定性。
		if why := describeUnencrypted(ct); why != "" {
			lastErr = errs.Newf("wecom: media body is not ciphertext (%s, %dB, content-type=%q, head=%q)",
				why, len(ct), ctType, printableHead(ct, 64))
			dumpFailedMedia(ctx, msgID, it.Type, ct)
			return nil, lastErr
		}
		pt, err := decryptMedia(ct, it.AESKey)
		if err == nil {
			return pt, nil
		}
		lastErr = errs.Wrapf(err, "wecom: decrypt %s failed (ct=%dB, content-type=%q)", it.Type, len(ct), ctType)
		dumpFailedMedia(ctx, msgID, it.Type, ct)
		if attempt+1 < wecomMediaAttempts {
			traceid.L(ctx).Warnw("wecom: media decrypt failed, retrying download once",
				"channel", c.name, "msgid", msgID, "type", it.Type, "bytes", len(ct), "err", err)
			if serr := sleepCtx(ctx, wecomMediaRetryDelay); serr != nil {
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// downloadMedia 拉一次密文，同时返回响应声明的 Content-Type（排查「返回的不是密文」时要用）。
func (c *WeComChannel) downloadMedia(ctx context.Context, it mediaItem) ([]byte, string, error) {
	if err := validateMediaURL(it.URL, c.cfg.MediaAllowedHosts); err != nil {
		return nil, "", err
	}
	dlCtx, cancel := context.WithTimeout(ctx, wecomMediaTimeout)
	defer cancel()

	// ⚠️ 走专门的 mediaHC：util/http 默认 maxBodySize 是 10MB，超限**不报错而是静默截断**，
	// 截断后的密文长度不是 16 的整数倍、解出来也是脏数据 —— 表现为「图片偶尔解不开」，
	// 极难定位。这里把上限放宽到与 wecomMediaMaxBytes 一致。
	resp, err := c.mediaHC.Get(it.URL).SetContext(dlCtx).Do()
	if err != nil {
		return nil, "", errs.Wrapf(err, "wecom: download %s", it.Type)
	}
	// ⚠️ 不判 resp.StatusCode：util/http 的 Do() 对任何非 2xx 都已返回非 nil error，
	// 写在这里的分支永远走不到（见项目记忆 util/http/client.go 的坑）。
	ct := resp.Body
	ctType := resp.Headers.Get("Content-Type")
	if len(ct) == 0 {
		return nil, ctType, errors.New("wecom: empty media body")
	}
	if len(ct) > wecomMediaMaxBytes {
		return nil, ctType, errs.Newf("wecom: media too large: %d bytes (cap %d)", len(ct), wecomMediaMaxBytes)
	}
	return ct, ctType, nil
}

// describeUnencrypted 判断下载到的 body 是不是**明文/已知文件格式**——
// 真密文是随机二进制，不可能带 gzip / PNG / ZIP / 文本这些特征。
// 命中说明下载到了错误页或服务端直出，重试也没用。返回 "" 表示看着像密文。
func describeUnencrypted(ct []byte) string {
	if len(ct) < 4 {
		return ""
	}
	switch {
	case bytes.HasPrefix(ct, []byte{0x1f, 0x8b}):
		return "gzip"
	case bytes.HasPrefix(ct, []byte{0x89, 'P', 'N', 'G'}):
		return "png"
	case bytes.HasPrefix(ct, []byte{0xff, 0xd8, 0xff}):
		return "jpeg"
	case bytes.HasPrefix(ct, []byte{'P', 'K', 0x03, 0x04}):
		return "zip"
	case bytes.HasPrefix(ct, []byte{'R', 'I', 'F', 'F'}):
		return "riff"
	case bytes.HasPrefix(ct, []byte{'%', 'P', 'D', 'F'}):
		return "pdf"
	}
	if sniff := nethttp.DetectContentType(ct); strings.HasPrefix(sniff, "text/") {
		return "text body (" + sniff + ")"
	}
	return ""
}

// printableHead 取 body 开头若干字节的可打印形式，用来一眼认出错误页。
func printableHead(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			sb.WriteString("\\x")
			sb.WriteString(hex.EncodeToString([]byte{c}))
		}
	}
	return sb.String()
}

// dumpFailedMedia 把解不开的密文落盘，供离线分析。
// 默认关闭，只有 WECOM_MEDIA_DUMP=1 时启用——入站媒体是用户隐私，不默认写盘。
func dumpFailedMedia(ctx context.Context, msgID, typ string, ct []byte) {
	if os.Getenv(wecomMediaDumpEnv) != "1" {
		return
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("wecom_media_fail_%s_%s.bin", sanitizeID(typ), sanitizeID(msgID)))
	if err := os.WriteFile(path, ct, 0o600); err != nil {
		return
	}
	traceid.L(ctx).Warnw("wecom: dumped undecryptable media for offline analysis",
		"msgid", msgID, "type", typ, "path", path, "bytes", len(ct))
}

// decryptMedia 解密企微临时素材：AES-256-CBC，IV 取密钥前 16 字节，去 PKCS#7 填充。
//
// ⚠️ aeskey 是 43 字符的**无 padding** base64（实测），直接
// base64.StdEncoding.DecodeString 会报 illegal base64 data —— 必须先补 "="。
// ⚠️ 填充粒度是 32 不是 16，见 wecomPadBlock（踩过：一半的媒体解不开）。
// 配方已在真实 PNG（928×1232）与 722KB 文本文件上验证过，见 docs §9.2.1。
func decryptMedia(ct []byte, aeskey string) ([]byte, error) {
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errs.Newf("wecom: ciphertext length %d is not a multiple of %d", len(ct), aes.BlockSize)
	}
	key, err := decodeUnpaddedBase64(aeskey)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errs.Newf("wecom: aeskey decodes to %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errs.Wrap(err, "wecom: aes new cipher")
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(out, ct)
	pt, err := pkcs7Unpad(out, wecomPadBlock)
	if err != nil {
		// 去填充失败基本等于「密钥或密文不对」。把现场证据全塞进 error，
		// 否则只剩一句 "invalid pkcs7 padding size N"，无从判断是哪一侧错了。
		return nil, errs.Wrapf(err, "wecom: ct=%dB pad=0x%02x", len(ct), out[len(out)-1]) // 只记长度与 pad 字节，不记明文
	}
	return pt, nil
}

// headHex / tailHex 取字节切片首尾的 hex 快照，供解密失败时定位。
func headHex(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return hex.EncodeToString(b)
}

func tailHex(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return hex.EncodeToString(b)
}

// decodeUnpaddedBase64 补齐 "=" 后再解码，兼容 43 字符的无 padding base64。
func decodeUnpaddedBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}

// pkcs7Unpad 去掉 PKCS#7 填充，并校验填充字节合法性（防误判）。
//
// ⚠️ maxPad 是**填充粒度**，不是 AES 块长：长度按 aes.BlockSize(16) 校验，
// 填充值按 maxPad(32) 校验（见 wecomPadBlock）。两者混用会让一半的文件解不开。
func pkcs7Unpad(b []byte, maxPad int) ([]byte, error) {
	if len(b) == 0 || len(b)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: invalid padded length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > maxPad || n > len(b) {
		return nil, errs.Newf("wecom: invalid pkcs7 padding size %d (max %d)", n, maxPad)
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("wecom: invalid pkcs7 padding bytes")
		}
	}
	return b[:len(b)-n], nil
}

// attachmentType 把企微 msgtype + 嗅探出的 MIME 映射到 core 的附件类型。
func attachmentType(msgType, mime string) string {
	switch msgType {
	case MsgTypeImage:
		return core.AttachmentTypeImage
	case MsgTypeVoice:
		return core.AttachmentTypeAudio
	case MsgTypeVideo:
		return core.AttachmentTypeVideo
	}
	// file / mixed：按 MIME 再判一次，嗅探出图片就算图片（能让多模态链路生效）。
	switch {
	case strings.HasPrefix(mime, "image/"):
		return core.AttachmentTypeImage
	case strings.HasPrefix(mime, "audio/"):
		return core.AttachmentTypeAudio
	case strings.HasPrefix(mime, "video/"):
		return core.AttachmentTypeVideo
	}
	return core.AttachmentTypeFile
}

// mediaFilename 造一个文件名。企微不给文件名，只能靠 msgtype + MIME 猜扩展名。
//
// ⚠️ 扩展名不是装饰：落工作空间后模型要用 `read_file` 打开它，
// 而 read_file 与各类工具常按扩展名判类型。text/plain 落成 `.bin`
// 会让模型（和人）都读不出来 —— 所以 text/* 必须有 .txt 之类的正确后缀。
func mediaFilename(msgType, mime, msgID string, idx int) string {
	mime = strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]) // 去掉 "; charset=utf-8"
	ext := ""
	switch {
	case strings.HasPrefix(mime, "image/"):
		ext = strings.TrimPrefix(mime, "image/")
		if ext == "jpeg" {
			ext = "jpg"
		}
	case strings.HasPrefix(mime, "audio/"):
		ext = strings.TrimPrefix(mime, "audio/")
	case strings.HasPrefix(mime, "video/"):
		ext = strings.TrimPrefix(mime, "video/")
	default:
		if e, ok := mimeExt[mime]; ok {
			ext = e
		}
	}
	if ext == "" || strings.Contains(ext, "/") {
		ext = "bin"
	}
	short := sanitizeID(msgID)
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "msg"
	}
	return fmt.Sprintf("wecom-%s-%d.%s", short, idx, ext)
}

// mimeExt 是「嗅探出的 MIME → 扩展名」的兜底表。
// DetectContentType 只认很有限的一批类型，且 text/* 一律给 text/plain，
// 所以文本文件只能落到 .txt（比 .bin 有用得多）。
var mimeExt = map[string]string{
	"text/plain":                    "txt",
	"text/html":                     "html",
	"text/csv":                      "csv",
	"application/json":              "json",
	"application/xml":               "xml",
	"application/pdf":               "pdf",
	"application/zip":               "zip",
	"application/x-gzip":            "gz",
	"application/x-tar":             "tar",
	"application/msword":            "doc",
	"application/vnd.ms-excel":      "xls",
	"application/vnd.ms-powerpoint": "ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   "docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
}

// formatSinkFile 把「已落到工作空间的文件」渲染成给 LLM 看的提示（含可读路径）。
//
// ⚠️ 工具名写 **read_file**（不带 sandbox_ 前缀）：sandbox 包注册时确实叫
// `sandbox_read_file`，但 `llm.stripSandboxPrefixes` 在把工具列表交给模型前会
// 剥掉 `sandbox_` 前缀，模型看到的名字就是 `read_file`。提示词里写
// `sandbox_read_file` 反而会让模型去调一个它根本看不到的工具名。
//
// 反过来，**权限规则里必须写 `sandbox_*`**：工具权限过滤（ResolveTools）发生在
// 剥前缀之前，用的是注册名。两处名字不一致是有意的，不是笔误。
func formatSinkFile(name, mime, rel string, n int) string {
	return fmt.Sprintf("[收到文件 %s（%s，%s），已保存到工作空间：%s —— 用 read_file 读取内容]",
		name, mime, humanBytes(n), rel)
}

// isTextualMIME 判断是否值得把内容内联进正文。
func isTextualMIME(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/csv", "application/toml",
		"application/x-sh", "application/sql":
		return true
	}
	return false
}

// formatInlineFile 把文本文件内容渲染成给 LLM 看的块。
func formatInlineFile(name, mime, content string) string {
	return fmt.Sprintf("[收到文件 %s（%s），内容如下]\n%s", name, mime, content)
}

// humanBytes 渲染人类可读的大小。
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// sleepCtx 等待 d，ctx 取消时提前返回 ctx.Err()。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sanitizeID 只保留 [A-Za-z0-9_-]，用于拼进文件名的外部标识（防路径穿越）。
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
