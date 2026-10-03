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

const conductNote = `Conduct: recalled memories are background for how you act, never evidence in an argument and never something to quote back. If the user says not to mention a subject again, acknowledge in one short sentence and do not name that subject, including in apologies. Do not rank another project until you have actually read its code. After the user corrects you, answer in one or two sentences.`

func ConductNote() string { return conductNote }
