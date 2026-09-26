package memory

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// ============================================================================
// Think 标签过滤器 — 在记忆写入前清理 LLM 深度思考内容
//
// 某些 LLM（如 DeepSeek-R1、GLM、QwQ 等）会将推理过程以 <think>...</think>
// 或 <thinking>...</thinking> 标签嵌入到回复文本中。这些推理内容对人类用户
// 没有直接价值，存储到记忆中会浪费存储空间和检索时的 token 预算。
//
// 本模块在写入记忆前移除这些标签及其内容，仅保留最终回复文本。
// 实现参考了 Memoh 项目的 FilterThinkingTags / FilterReasoningArray。
// ============================================================================

// thinkTagRe 匹配 <think>...</think> 和 <thinking>...</thinking> 块。
// 标志说明：
//   - i: 不区分大小写（某些模型输出 <Think> 或 <THINKING>）
//   - s: 使 . 匹配换行符（思考内容通常是多行的）
var thinkTagRe = regexp.MustCompile(`(?is)<think(?:ing)?>\s*.*?\s*</think(?:ing)?>`)

// unclosedThinkRe 匹配只有开标签没有闭标签的 <think>/<thinking>（流式截断场景）。
var unclosedThinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*$`)

// StripThinkTags 从文本中移除 <think>...</think> 和 <thinking>...</thinking> 块。
//
// 处理逻辑：
//  1. 移除完整的 think/thinking 标签对及其内容
//  2. 移除未闭合的 think/thinking 开标签（截断的流式输出）
//  3. 清理多余空白，返回 TrimSpace 后的结果
//
// 如果移除后内容为空（即文本只包含思考内容），返回空字符串。
func StripThinkTags(text string) string {
	cleaned := thinkTagRe.ReplaceAllString(text, "")
	cleaned = unclosedThinkRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

// internalTagRe 匹配 <internal>...</internal> 块（回复控制协议要求的私有心里话标签）。
// 标志与 thinkTagRe 一致：i 不区分大小写，s 使 . 匹配换行（心里话通常多行）。
var internalTagRe = regexp.MustCompile(`(?is)<internal>\s*.*?\s*</internal>`)

// unclosedInternalRe 匹配只有开标签没有闭标签的 <internal>（流式截断场景）。
// 注意：调用方只会把「控制块之前的内容」传入，故 .*$ 不会吞掉 @@REPLY_CONTROL@@ 控制行。
var unclosedInternalRe = regexp.MustCompile(`(?is)<internal>.*$`)

// StripInternalTags 从文本中移除 <internal>...</internal> 私有心里话标签及其内容。
//
// 出站链路用它确保模型写在 <internal> 里的判断、吐槽、内部备注永不外发——
// 这是「同时输出心里话和想说的话」场景下防止心里话泄漏的关键一道。
// 与 StripThinkTags 同范式：先去完整标签对，再去未闭合开标签，最后清理空白。
func StripInternalTags(text string) string {
	cleaned := internalTagRe.ReplaceAllString(text, "")
	cleaned = unclosedInternalRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

// reasoningPart 对应某些 API（如智谱 GLM）在 content 字段中发出的 JSON 推理数组。
type reasoningPart struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

// StripReasoningArray 检测并剥离原始 JSON 推理数组。
//
// 某些 API（如智谱 GLM）在上下文溢出或特殊模式下，会在 content 字段中
// 发出形如 [{"text":"...","type":"reasoning"},{"text":"...","type":"text"}]
// 的 JSON 数组，而非普通文本。
//
// 本函数提取其中 type="text" 的部分并用换行连接；
// 如果输入不是推理数组格式，则原样返回。
func StripReasoningArray(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[{") || !strings.HasSuffix(trimmed, "}]") {
		return text
	}

	var parts []reasoningPart
	if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
		return text
	}
	if len(parts) == 0 {
		return text
	}

	hasReasoning := false
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "reasoning":
			hasReasoning = true
		default:
			// 未知类型，不是推理数组
			return text
		}
	}

	if !hasReasoning {
		return text
	}

	return strings.Join(texts, "\n")
}

// StripThinking 对文本执行完整的思考内容清理。
// 依次应用 StripReasoningArray → StripThinkTags。
// 这是记忆写入前应调用的主入口。
func StripThinking(text string) string {
	text = StripReasoningArray(text)
	return StripThinkTags(text)
}

// ============================================================================
// 内部状态泄露过滤器 — 防止系统提示中的内部指标外泄到公开回复
// ============================================================================

// internalCharsRe 匹配形如 "(2,206/2,200 字符)" 的内部用量标记
// （来自记忆块头部的 [current/limit chars]）。
var internalCharsRe = regexp.MustCompile(`\(\s*\d[\d,]*\s*\/\s*\d[\d,]*\s*字符\s*\)`)

// internalCharsEnRe 匹配形如 "2,206/2,200 chars" 的英文内部用量标记。
var internalCharsEnRe = regexp.MustCompile(`\d[\d,]*\s*\/\s*\d[\d,]*\s*chars?`)

// internalPhraseRe 匹配直接复述内部状态的固定短语（模型可能把系统提示里的
// 记忆容量指标 paraphrase 成中文写进公开回复）。
var internalPhraseRe = regexp.MustCompile(`当前记忆已接近容量上限|记忆容量上限|记忆已接近容量上限|接近容量上限`)

// StripInternalState 从最终回复文本中剥离内部系统状态（记忆用量指标等），
// 防止「心里话 / 内部指标」泄漏到公开帖文。
//
// 典型泄漏案例：bot 把系统提示里的 "[2,206/2,200 chars]" 复述成
// 「当前记忆已接近容量上限（2,206/2,200 字符）」公开发到时间线。
// 本函数在 llmroute 出站清洗阶段（StripThinking 之后）调用，作为纵深防御。
//
// 空白处理只作用于「被剥离处」的接缝：原先对全文执行 \s{2,} → " "，会把没有任何
// 内部指标的正常回复里的段落空行（\n\n）和代码缩进一并压扁，Telegram / Misskey /
// Web 等所有渠道都会丢失排版。现在仅在删除点两侧折叠水平空白，其余原样保留。
func StripInternalState(text string) string {
	text = removeAndJoin(internalCharsRe, text)
	text = removeAndJoin(internalCharsEnRe, text)
	text = removeAndJoin(internalPhraseRe, text)
	return strings.TrimSpace(text)
}

// removeAndJoin 删除 re 的所有匹配，并只在删除点把两侧的水平空白（空格 / Tab）
// 折叠为至多一个空格；换行两侧不补空格。未命中时原样返回。
func removeAndJoin(re *regexp.Regexp, text string) string {
	locs := re.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, loc := range locs {
		b.WriteString(text[prev:loc[0]])
		prev = loc[1]
		left := b.String()
		lTrim := strings.TrimRight(left, " \t")
		rest := text[prev:]
		rTrim := strings.TrimLeft(rest, " \t")
		if len(lTrim) == len(left) && len(rTrim) == len(rest) {
			continue // 删除点两侧都没有空白：直接拼接
		}
		b.Reset()
		b.WriteString(lTrim)
		if lTrim != "" && rTrim != "" && !strings.HasSuffix(lTrim, "\n") && !strings.HasPrefix(rTrim, "\n") {
			b.WriteByte(' ')
		}
		prev += len(rest) - len(rTrim)
	}
	b.WriteString(text[prev:])
	return b.String()
}

// ============================================================================
// 上下文标记过滤器 — 防止入站注入的上下文标记泄漏到对外回复
// ============================================================================

// contextMarkerRe 匹配入站阶段注入的「上下文标记」整行，形态有三种：
//   - [Reply to <sender>: <quoted>]  —— noteContext 在用户回复某帖时前置，告知模型"用户在回复谁/回复了啥"
//   - [Renote from <sender>: <quoted>] —— 同上，引用转发场景
//   - [note_id: <id>] —— 当前帖 ID，供模型调引用/反应类工具
//
// 这些标记仅供模型理解上下文，绝不应出现在对外发送的回复正文里。
// 模型偶发会把入站文本里的 [Reply to ...] / [Renote from ...] 原样回显到自己的回复开头
// （实测 GLM 在 reply 场景把 `[Reply to @luna: 嘿嘿]` 直接复制成了回复首行），
// 导致对外帖子出现诡异的方括号前缀。这与 StripThinking / StripInternalState 同一思路：
// 在出站清洗阶段兜底剥离，而非完全依赖模型遵守提示词。
//
// 匹配整行（允许行首尾空白），不误伤正文里普通出现的方括号内容。
var contextMarkerRe = regexp.MustCompile(
	`(?m)^\s*\[(?:(?:Reply to|Renote from) .*?: .*?|note_id: .*?)\]\s*$`,
)

// StripContextMarkers 从最终回复文本中剥离入站注入的上下文标记整行，
// 防止 `[Reply to ...]` / `[Renote from ...]` / `[note_id: ...]` 泄漏到公开帖文。
//
// 在 llmroute 出站清洗阶段（StripThinking / StripInternalState 之后）调用，
// 作为纵深防御的最后一道兜底。
func StripContextMarkers(text string) string {
	return strings.TrimSpace(contextMarkerRe.ReplaceAllString(text, ""))
}

// ============================================================================
// 短内容过滤器 — 在记忆写入前剔除过短/过碎的噪声内容
// ============================================================================

// 短内容过滤阈值。
//   - MinMemoryChars：去空白后 rune 数下限（任意语言下极短内容，如 "ok"、"好的"）。
//   - MinMemoryWords：词数下限（空格分隔内容下的短句，如 "thanks a lot"）。
//
// 满足任一即视为琐碎噪声，不应作为长期记忆存储。
const (
	MinMemoryChars = 5
	MinMemoryWords = 5
)

// IsTrivialMemoryContent 判断文本是否「过短或低信息、不值得作为长期记忆存储」。
//
// 判定（满足任一即琐碎）：
//   - 去首尾空白后 rune 数 < MinMemoryChars
//   - 词数 < MinMemoryWords
//   - Misskey 投票 bot 刷屏（投票提问模板或其「无投票」回声）
//   - Misskey 表情短码主导（>=2 个 :name: 且剥离后几乎无实义字符）
//   - 短内容重复符号主导（总长 <= MaxRepetitionLen 且重复标点/filler 假名
//     占去空白正文的过半）
//
// 词数统计对 CJK 字符「逐字成词」、非 CJK 片段按空白切分（见 countWords），
// 因此纯中文只需满足字符数下限（不会被英文词数规则误杀），纯英文需满足词数下限
// （防止 "yes" / "lol" / "ok" 这类随口短回复进入记忆，降低 dreaming / 记忆系统噪声）。
//
// 后三类为针对真实库内噪声（详见 thinkbot 记忆清理实践）补充的高精确率规则：
// 投票 bot 帖、表情短码刷屏、以及 "ふえええええ！？" 这类纯情绪重复——它们无长期
// 记忆价值。长内容（> MaxRepetitionLen）即使含重复符号也一律放过，避免误删真实记忆
// （如 "SEKIRO 弦一郎直前まで進められたぞ！！！" 这类带情绪的合法内容）。
//
// 典型用例：note_capture 捕获用户发言、MemoryWriteStage 落库、backfill 事件流回灌，
// 写入前调用本函数过滤，避免低质短内容污染长期记忆与梦境巩固输入；cleanup-trivial
// 运维接口也复用它来识别存量垃圾。
func IsTrivialMemoryContent(text string) bool {
	s := strings.TrimSpace(text)
	runes := []rune(s)
	if len(runes) < MinMemoryChars {
		return true
	}
	if countWords(runes) < MinMemoryWords {
		return true
	}

	// 投票 bot 刷屏：Misskey 投票帖提问模板，或其无投票时的回声。
	// 这类由 bot 自动发出，对用户长期记忆无价值。
	if misskeyPollEchoRe.MatchString(s) || misskeyPollQRe.MatchString(s) {
		return true
	}

	// 表情短码主导：>=2 个 :name: 短码且剥离短码/URL/空白后几乎无实义字符
	// （如 ":panpan::panpan:…"）。属于历史库中出现的表情刷屏类噪声。
	if sc := emojiShortcodeRe.FindAllString(s, -1); len(sc) >= 2 {
		stripped := emojiShortcodeRe.ReplaceAllString(s, "")
		stripped = urlRe.ReplaceAllString(stripped, "")
		stripped = strings.Join(strings.Fields(stripped), "")
		meaningful := 0
		for _, r := range stripped {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				meaningful++
			}
		}
		if meaningful <= 2 {
			return true
		}
	}

	// 短内容重复符号主导：总长受限且重复标点/filler 假名占去空白正文过半。
	// 仅对短内容生效，保护带情绪的合法长文。
	if len(runes) <= MaxRepetitionLen {
		nonspace, noise := 0, 0
		for _, r := range s {
			if !unicode.IsSpace(r) {
				nonspace++
			}
		}
		// 手动扫描连续相同字符的 run（RE2 不支持反向引用，无法用正则表达 \1）。
		rs := []rune(s)
		for i := 0; i < len(rs); {
			j := i + 1
			for j < len(rs) && rs[j] == rs[i] {
				j++
			}
			runLen := j - i
			if runLen >= 4 && isFillerRunChar(rs[i]) {
				noise += runLen
			}
			i = j
		}
		if noise >= 4 && noise >= nonspace/2 {
			return true
		}
	}

	return false
}

// MaxRepetitionLen 是「重复符号主导」检测的生效长度上限（rune 数）。
// 超过此长度的内容即使含重复符号也放过，避免误删带情绪的合法长文。
const MaxRepetitionLen = 40

// misskeyPollQRe 匹配 Misskey 投票 bot 的投票提问模板
// 「みなさんは、<任意内容>と思いますか？」——bot 自动发出的投票帖，无长期记忆价值。
var misskeyPollQRe = regexp.MustCompile(`みなさんは、.+と思いますか？$`)

// misskeyPollEchoRe 匹配投票 bot 在无投票时的回声「投票はありませんでした」，
// 常出现在 "[Renote from ...: <投票提问>]\n投票はありませんでした" 形态里。
var misskeyPollEchoRe = regexp.MustCompile(`投票はありませんでした`)

// emojiShortcodeRe 匹配 Misskey 表情短码 :name:。
var emojiShortcodeRe = regexp.MustCompile(`:[A-Za-z0-9_]+:`)

// urlRe 匹配 URL，避免把链接里的 "www" / 路径误判为 emoji 短码或重复噪声。
var urlRe = regexp.MustCompile(`https?://\S+|www\.\S+`)

// isFillerRunChar 判断重复字符是否属于「低信息噪声」集合：
//   - 标点（。、！？… 等）的连续重复是刷屏式标点
//   - 情绪 filler 假名/汉字（笑笑/哈哈/草草/ええ/っっ 等）的连续重复是纯情绪噪声
//
// 刻意排除长音 ー、波浪号 〜、字母、数字：它们常出现在合法日文长音 elongation、
// URL、代码、base64 中，误判成本高。保护真实记忆优先于多抓几条噪声。
func isFillerRunChar(r rune) bool {
	switch r {
	case '。', '、', '，', '！', '？', '!', '?', '…', '・', '♪',
		'笑', '哈', '草', '哇', 'え', 'う', 'ん', 'ね', 'ふ', 'む', 'お', 'っ', 'ぃ':
		return true
	}
	return false
}

// isCJK 判断 rune 是否属于中日韩表意/假名/谚文文字（逐字成词计数）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// countWords 统计 CJK 感知词数：
//   - 连续 CJK 字符：每个字独立计 1 词（"今天天气" → 4 词）
//   - 非 CJK 连续段：按空白切分，每段计 1 词（"hello world" → 2 词）
//
// 中英混排内容（如 "今天 hello world"）会同时计入 CJK 字数与英文词数，
// 既不会因中文无空格而词数过低误杀，也不会放过高噪声英文短语。
func countWords(runes []rune) int {
	words := 0
	var latin []rune
	flush := func() {
		if len(latin) > 0 {
			words++
			latin = latin[:0]
		}
	}
	for _, r := range runes {
		if isCJK(r) {
			flush()
			words++
		} else if unicode.IsSpace(r) {
			flush()
		} else {
			latin = append(latin, r)
		}
	}
	flush()
	return words
}

// ============================================================================
// ThinkFilterStore — 自动清理思考内容的 Store 装饰器
// ============================================================================

// ThinkFilterStore 包装一个底层 Store，在 Append 前自动对 Entry.Content
// 执行 StripThinking 清理。
//
// 使用方式：
//
//	repo := memory.NewMemoryRepository()
//	filtered := memory.NewThinkFilterStore(repo)
//	// filtered 满足 Store 接口，后续所有 Append 都会自动清理 think 标签
type ThinkFilterStore struct {
	inner Store
}

// NewThinkFilterStore 创建思考内容过滤 Store 装饰器。
func NewThinkFilterStore(inner Store) *ThinkFilterStore {
	return &ThinkFilterStore{inner: inner}
}

// Append 在写入前清理 Entry.Content 中的思考内容。
func (s *ThinkFilterStore) Append(ctx context.Context, entry Entry) error {
	entry.Content = StripThinking(entry.Content)
	return s.inner.Append(ctx, entry)
}

// Delete 透传到底层 Store。
func (s *ThinkFilterStore) Delete(ctx context.Context, scope Scope, entryID string) error {
	return s.inner.Delete(ctx, scope, entryID)
}

// Clear 透传到底层 Store。
func (s *ThinkFilterStore) Clear(ctx context.Context, scope Scope) error {
	return s.inner.Clear(ctx, scope)
}
