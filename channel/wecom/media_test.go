package wecom

// 媒体解密单测。
//
// 加解密必须是**可往返验证**的：企微的 aeskey 是 43 字符无 padding base64，
// 一旦哪天对端改成带 padding 或换 IV 规则，这里会第一个炸，而不是等到线上
// 「用户发图、bot 说没收到」。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// encryptLikeWeCom 按企微的配方加密，用于构造往返样本。
func encryptLikeWeCom(t *testing.T, plain, key []byte) (ct []byte, aeskey string) {
	t.Helper()
	if len(key) != 32 {
		t.Fatalf("key must be 32 bytes, got %d", len(key))
	}
	// PKCS#7 填充
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append([]byte{}, plain...)
	buf = append(buf, bytes.Repeat([]byte{byte(pad)}, pad)...)

	out := make([]byte, len(buf))
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, buf)
	// 企微给的是 43 字符无 padding base64
	return out, strings.TrimRight(base64.StdEncoding.EncodeToString(key), "=")
}

func TestDecryptMediaRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain := []byte("hello 企业微信 媒体解密 🖼️")

	ct, aeskey := encryptLikeWeCom(t, plain, key)
	if len(aeskey) != 43 {
		t.Fatalf("aeskey len = %d, want 43 (unpadded base64 of 32 bytes)", len(aeskey))
	}

	got, err := decryptMedia(ct, aeskey)
	if err != nil {
		t.Fatalf("decryptMedia: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, plain)
	}
}

func TestDecryptMediaRejectsGarbage(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ct, aeskey := encryptLikeWeCom(t, []byte("x"), key)

	// 密文长度不是块大小的整数倍
	if _, err := decryptMedia(ct[:len(ct)-1], aeskey); err == nil {
		t.Fatal("expected error for misaligned ciphertext")
	}
	// 密钥长度不对（16 字节）
	short := base64.StdEncoding.EncodeToString(key[:16])
	if _, err := decryptMedia(ct, strings.TrimRight(short, "=")); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
	// 填充字节非法：解密出来的填充值不合法时不应静默返回脏数据
	if _, err := decryptMedia(ct, aeskey); err != nil {
		t.Fatalf("valid sample should decrypt: %v", err)
	}
}

func TestDecodeUnpaddedBase64(t *testing.T) {
	// 43 字符无 padding —— 企微的真实形态
	if _, err := decodeUnpaddedBase64(strings.Repeat("A", 43)); err != nil {
		t.Fatalf("43-char unpadded should decode: %v", err)
	}
	// 已带 padding 也不能炸（将来对端改格式时仍兼容）
	if _, err := decodeUnpaddedBase64(strings.Repeat("A", 44) + "=="); err == nil {
		t.Fatal("44 chars + == is invalid base64, expected error")
	}
}

func TestAttachmentTypeAndFilename(t *testing.T) {
	if got := attachmentType(MsgTypeImage, "image/png"); got != "image" {
		t.Fatalf("image → %q", got)
	}
	if got := attachmentType(MsgTypeVoice, "audio/amr"); got != "audio" {
		t.Fatalf("voice → %q", got)
	}
	if got := attachmentType(MsgTypeFile, "image/png"); got != "image" {
		t.Fatalf("file with image mime → %q, want image (让多模态链路生效)", got)
	}
	if got := attachmentType(MsgTypeFile, "application/octet-stream"); got != "file" {
		t.Fatalf("file → %q", got)
	}

	name := mediaFilename(MsgTypeFile, "image/jpeg", "deadbeefcafe1234", 0)
	if !strings.HasPrefix(name, "wecom-") || !strings.HasSuffix(name, ".jpg") {
		t.Fatalf("filename = %q, want wecom-<short>-0.jpg", name)
	}
	if name := mediaFilename(MsgTypeFile, "application/octet-stream", "abc", 1); !strings.HasSuffix(name, ".bin") {
		t.Fatalf("filename = %q, want .bin fallback", name)
	}
}

func TestMediaFilenameUsesRealExtension(t *testing.T) {
	// ⚠️ 扩展名不是装饰：落工作空间后模型用 read_file 打开它，
	// 而部分工具按扩展名判类型。text/plain 落成 .bin 会让模型读不出来。
	cases := []struct {
		mime string
		want string
	}{
		{"text/plain; charset=utf-8", "txt"},
		{"text/plain", "txt"},
		{"application/json", "json"},
		{"image/jpeg", "jpg"},
		{"application/octet-stream", "bin"},
	}
	for _, tc := range cases {
		got := mediaFilename(MsgTypeFile, tc.mime, "abcdefgh12345678", 0)
		if !strings.HasSuffix(got, "."+tc.want) {
			t.Fatalf("mediaFilename(%q) = %q, want suffix .%s", tc.mime, got, tc.want)
		}
	}
}

// TestDescribeUnencrypted 覆盖「下载到的不是密文」的识别。
// 2026-10-20 线上出现过 `invalid pkcs7 padding size 32` —— 只报 padding 无从判断
// 是密钥错还是拿到的根本不是密文（错误页 / 服务端直出）。这个识别是排查的入口。
func TestDescribeUnencrypted(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00, 0x11, 0x22}, "gzip"},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}, "png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}, "jpeg"},
		{"zip", []byte{'P', 'K', 0x03, 0x04, 0x14}, "zip"},
		{"html error page", []byte("<html><body>404 not found</body></html>"), "text body"},
		{"json error", []byte(`{"errcode":40001,"errmsg":"invalid"}`), "text body"},
		{"real ciphertext", make([]byte, 64), ""}, // 全 0 也是合法的"看着像密文"
	}
	for _, tc := range cases {
		if tc.name == "real ciphertext" {
			// 全 0 体：DetectContentType 会给 application/octet-stream
			for i := range tc.body {
				tc.body[i] = byte(i * 37 & 0xff)
			}
		}
		got := describeUnencrypted(tc.body)
		if tc.want == "" {
			if got != "" {
				t.Fatalf("%s: describeUnencrypted = %q, want \"\"", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%s: describeUnencrypted = %q, want contains %q", tc.name, got, tc.want)
		}
	}
}

// TestPrintableHead 保证非 ASCII 不被直接塞进日志（否则 \x89PNG 之类不可见，
// 排查时看不出到底拿到了什么）。
func TestPrintableHead(t *testing.T) {
	got := printableHead([]byte{0x89, 'P', 'N', 'G', 'x'}, 8)
	if !strings.Contains(got, "\\x89") || !strings.Contains(got, "PNG") {
		t.Fatalf("printableHead = %q, want escaped \\x89 plus readable PNG", got)
	}
	if len(printableHead([]byte("abcdefghij"), 4)) > 4*6 {
		t.Fatalf("printableHead should truncate to n bytes")
	}
}

func TestHeadTailHex(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := headHex(b, 3); got != "010203" {
		t.Fatalf("headHex = %q", got)
	}
	if got := tailHex(b, 3); got != "08090a" {
		t.Fatalf("tailHex = %q", got)
	}
	// 短于 n 时不要 panic 也不要补齐
	if got := tailHex([]byte{0xff}, 16); got != "ff" {
		t.Fatalf("tailHex(short) = %q", got)
	}
}

// TestDecryptMediaPaddingGranularityIs32 锁死「填充粒度 32 字节」这个反直觉的事实。
//
// 背景：企微官方 PKCS7Encoder 的 block_size 是 **32**，不是 AES 的 16。
// 于是填充值 n 的取值是 1..32。按 16 校验时，凡 `明文长度 % 32 < 16` 的媒体都会被判成
// `invalid pkcs7 padding size N`（N 落在 17..32）—— 也就是**约一半的文件随机收不到**，
// 而另一半（L%32>=16）与标准 PKCS#7 重合，看起来完全正常，所以极难发现。
//
// 这里把 L%32 的 0..31 全跑一遍，确保 32 种长度都能往返。
func TestDecryptMediaPaddingGranularityIs32(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	for mod := 0; mod < 32; mod++ {
		plain := bytes.Repeat([]byte("a"), 64+mod) // 任意基准 + mod，覆盖 L%32 = mod
		ct, aeskey := encryptLikeWeCom32(t, plain, key)
		got, err := decryptMedia(ct, aeskey)
		if err != nil {
			t.Fatalf("L%%32=%d: decryptMedia: %v", mod, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("L%%32=%d: got %d bytes, want %d", mod, len(got), len(plain))
		}
	}
}

// TestDecryptMediaRejectsPaddingOver32 填充值 33 以上必须拒绝（防把垃圾当明文）。
func TestDecryptMediaRejectsPaddingOver32(t *testing.T) {
	key := make([]byte, 32)
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte(33) // 尾字节 33 → 超过 wecomPadBlock
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(ct, buf)
	aeskey := base64.RawStdEncoding.EncodeToString(key)
	if _, err := decryptMedia(ct, aeskey); err == nil {
		t.Fatal("decryptMedia should reject padding > 32")
	}
}

// encryptLikeWeCom32 按「填充到 32 字节边界」加密（企微的实际行为）。
func encryptLikeWeCom32(t *testing.T, plain, key []byte) (ct []byte, aeskey string) {
	t.Helper()
	// ⚠️ 粒度是 wecomPadBlock(32)，不是 aes.BlockSize(16)
	pad := wecomPadBlock - len(plain)%wecomPadBlock
	buf := append([]byte{}, plain...)
	buf = append(buf, bytes.Repeat([]byte{byte(pad)}, pad)...)

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, buf)
	return out, base64.RawStdEncoding.EncodeToString(key)
}
