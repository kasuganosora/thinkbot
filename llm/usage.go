package llm

// InputTokenDetail breaks down input token usage.
type InputTokenDetail struct {
	NoCacheTokens    int `json:"noCacheTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	// CacheWrite5mTokens is the number of tokens written to the 5-minute cache
	// (Anthropic-specific, populated when using cache_control with default TTL).
	CacheWrite5mTokens int `json:"cacheWrite5mTokens,omitempty"`
	// CacheWrite1hTokens is the number of tokens written to the 1-hour cache
	// (Anthropic-specific, populated when using cache_control with ttl="1h").
	CacheWrite1hTokens int `json:"cacheWrite1hTokens,omitempty"`
}

// OutputTokenDetail breaks down output token usage.
type OutputTokenDetail struct {
	TextTokens      int `json:"textTokens"`
	ReasoningTokens int `json:"reasoningTokens"`
}

// Usage tracks token consumption for a single generation call.
type Usage struct {
	InputTokens        int               `json:"inputTokens"`
	OutputTokens       int               `json:"outputTokens"`
	TotalTokens        int               `json:"totalTokens"`
	ReasoningTokens    int               `json:"reasoningTokens,omitempty"`
	CachedInputTokens  int               `json:"cachedInputTokens,omitempty"`
	InputTokenDetails  InputTokenDetail  `json:"inputTokenDetails,omitempty"`
	OutputTokenDetails OutputTokenDetail `json:"outputTokenDetails,omitempty"`
}

// Add accumulates another Usage into this one (mutates receiver).
func (u *Usage) Add(other *Usage) {
	if other == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.InputTokenDetails.NoCacheTokens += other.InputTokenDetails.NoCacheTokens
	u.InputTokenDetails.CacheReadTokens += other.InputTokenDetails.CacheReadTokens
	u.InputTokenDetails.CacheWriteTokens += other.InputTokenDetails.CacheWriteTokens
	u.InputTokenDetails.CacheWrite5mTokens += other.InputTokenDetails.CacheWrite5mTokens
	u.InputTokenDetails.CacheWrite1hTokens += other.InputTokenDetails.CacheWrite1hTokens
	u.OutputTokenDetails.TextTokens += other.OutputTokenDetails.TextTokens
	u.OutputTokenDetails.ReasoningTokens += other.OutputTokenDetails.ReasoningTokens
}
