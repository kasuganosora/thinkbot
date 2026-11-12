package llm

// WireSampling drops a nil value and the model-config sentinel (-1).
// Every other value, including 0, is sent as-is.
// Chat Completions, Responses, and the other provider encoders all use this
// so a configured -1 is never written onto the wire as a number.
func WireSampling(v *float64) *float64 {
	if v == nil || *v == OmitSamplingParam {
		return nil
	}
	return v
}
