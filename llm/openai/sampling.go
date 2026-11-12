package openai

import "github.com/kasuganosora/thinkbot/llm"

// wireSampling drops a nil value and the model-config sentinel (-1).
// Every other value, including 0, is sent as-is.
func wireSampling(v *float64) *float64 {
	if v == nil || *v == llm.OmitSamplingParam {
		return nil
	}
	return v
}
