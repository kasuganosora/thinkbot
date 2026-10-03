package memory

import (
	"strings"
	"unicode"
)

const suppressCategory = "suppress"

// BanTopic 从「别再提 X」这类句子里抽出不该再召回的主题。抽不到返回空。
func BanTopic(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	markers := []string{"别再提", "不要再提", "别提", "不准再提", "再提"}
	idx := -1
	markerLen := 0
	for _, m := range markers {
		if i := strings.Index(text, m); i >= 0 && (idx < 0 || i < idx) {
			idx = i
			markerLen = len(m)
		}
	}
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(text[idx+markerLen:])
	rest = strings.TrimLeftFunc(rest, func(r rune) bool {
		return r == '的' || r == '了' || unicode.IsSpace(r)
	})
	end := len(rest)
	for _, cut := range []string{" ", "，", ",", "。", "！", "!", "不然", "否则", "我就", "我要"} {
		if i := strings.Index(rest, cut); i >= 0 && i < end {
			end = i
		}
	}
	topic := strings.TrimSpace(rest[:end])
	topic = strings.Trim(topic, "「」\"'")
	for _, suffix := range []string{"的事情", "这件事", "这事", "的事"} {
		topic = strings.TrimSuffix(topic, suffix)
	}
	topic = strings.TrimSpace(topic)
	if len([]rune(topic)) < 2 || len([]rune(topic)) > 24 {
		return ""
	}
	return topic
}

// FilterSuppressed 去掉正文命中禁提主题的记忆。suppress 条目本身不注入。
func FilterSuppressed(entries []Entry, topics []string) []Entry {
	if len(entries) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Category == suppressCategory {
			continue
		}
		if topicHit(e.Content, topics) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func topicHit(content string, topics []string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, topic := range topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(topic)) {
			return true
		}
	}
	return false
}

// FilterUnrelated 丢掉和当前这句话对不上的记忆，避免聊别的主题时把旧伤疤灌进提示。
// 用户这句没提到的内容仍留在库里，搜索工具还能按原话找。
func FilterUnrelated(entries []Entry, text string) []Entry {
	text = strings.TrimSpace(text)
	if text == "" || len(entries) == 0 {
		return entries
	}
	grams := charGrams(text)
	if len(grams) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if sharesGram(e.Content, grams) {
			out = append(out, e)
		}
	}
	return out
}

func charGrams(text string) map[string]struct{} {
	runes := []rune(strings.ToLower(text))
	out := make(map[string]struct{})
	for i := 0; i < len(runes); i++ {
		if unicode.IsSpace(runes[i]) || unicode.IsPunct(runes[i]) {
			continue
		}
		out[string(runes[i:i+1])] = struct{}{}
		if i+1 < len(runes) {
			out[string(runes[i:i+2])] = struct{}{}
		}
	}
	return out
}

func sharesGram(content string, grams map[string]struct{}) bool {
	runes := []rune(strings.ToLower(content))
	for i := 0; i+1 < len(runes); i++ {
		if _, ok := grams[string(runes[i:i+2])]; ok {
			return true
		}
	}
	return false
}

const conductNote = `Conduct: recalled memories are background for how you act, never evidence in an argument and never something to quote back. Do not bring up the user's past failures, exams, relationship status, or other personal history unless they raised that subject in this message. If the user says not to mention a subject again, acknowledge in one short sentence and do not name that subject, including in apologies. Do not rank another project until you have actually read its code. After the user corrects you, answer in one or two sentences.`

func ConductNote() string { return conductNote }
