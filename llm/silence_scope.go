package llm

// SilenceRuleScope is appended to every memory-writing prompt (compaction,
// consolidation, compression, extraction, profiling, self-compaction). Models
// tended to record situational etiquette such as "not addressed to me, observe
// silently" or "keep reply frequency low" as unconditional rules, which later
// suppressed replies to people who @-mentioned the bot directly.
const SilenceRuleScope = `

Scope rule for behavioral conventions: whenever you write (or keep, or merge) any rule about staying silent, lurking, only observing, not interjecting, or limiting reply frequency, you MUST explicitly scope it, e.g. "applies only when the bot is not addressed; does NOT apply when the bot is directly @mentioned or someone replies to the bot's own post". Never write such a rule as unconditional.`
