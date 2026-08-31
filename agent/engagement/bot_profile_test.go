package engagement

import (
	"testing"
)

// ============================================================================
// ParseSoulProfile 测试
// ============================================================================

func TestParseSoulProfile_Default(t *testing.T) {
	traits := ParseSoulProfile("")
	if traits.EnergyLevel != 0.5 {
		t.Errorf("expected EnergyLevel=0.5, got %f", traits.EnergyLevel)
	}
	if traits.Patience != 0.6 {
		t.Errorf("expected Patience=0.6, got %f", traits.Patience)
	}
	if traits.Verbosity != 0.9 {
		t.Errorf("expected Verbosity=0.9, got %f", traits.Verbosity)
	}
	if traits.Confidence != 0.3 {
		t.Errorf("expected Confidence=0.3, got %f", traits.Confidence)
	}
}

func TestParseSoulProfile_EnthusiasticBot(t *testing.T) {
	content := `# Soul

You are an enthusiastic AI assistant.

## Personality

- enthusiastic and passionate about helping people
- energetic and proactive in conversations
- patient and tolerant with all users`

	traits := ParseSoulProfile(content)

	if traits.EnergyLevel != 0.9 {
		t.Errorf("enthusiastic bot should have EnergyLevel=0.9, got %f", traits.EnergyLevel)
	}
	if traits.Patience != 0.9 {
		t.Errorf("patient bot should have Patience=0.9, got %f", traits.Patience)
	}
}

func TestParseSoulProfile_QuietObserver(t *testing.T) {
	content := `# Soul

You are a quiet observer.

## Personality

- reserved and reactive
- quiet and cautious in discussions
- concise and direct in communication`

	traits := ParseSoulProfile(content)

	if traits.EnergyLevel != 0.3 {
		t.Errorf("quiet bot should have EnergyLevel=0.3, got %f", traits.EnergyLevel)
	}
	if traits.Verbosity != 0.4 {
		t.Errorf("concise bot should have Verbosity=0.4, got %f", traits.Verbosity)
	}
}

func TestParseSoulProfile_PersonalityExtraction(t *testing.T) {
	content := `# Soul

## Personality

- Friendly and approachable
- Concise and direct in communication
- Helpful and knowledgeable`

	traits := ParseSoulProfile(content)

	expectedPersona := "Friendly and approachable; Concise and direct in communication; Helpful and knowledgeable"
	if traits.Personality != expectedPersona {
		t.Errorf("expected persona %q, got %q", expectedPersona, traits.Personality)
	}
}

func TestParseSoulProfile_TopicsFromInterests(t *testing.T) {
	content := `# Soul

interests: Go, Rust, Kubernetes, Distributed Systems`

	traits := ParseSoulProfile(content)

	if len(traits.PreferredTopics) != 4 {
		t.Errorf("expected 4 topics, got %d: %v", len(traits.PreferredTopics), traits.PreferredTopics)
	}
}

func TestParseSoulProfile_VerboseBot(t *testing.T) {
	content := `# Soul

You are a verbose assistant who gives detailed and elaborate explanations.`

	traits := ParseSoulProfile(content)

	if traits.Verbosity != 1.0 {
		t.Errorf("verbose bot should have Verbosity=1.0, got %f", traits.Verbosity)
	}
}

func TestParseSoulProfile_FrontMatterProfile(t *testing.T) {
	content := `---
profile: {"energy_level":0.8,"patience":0.7,"verbosity":0.6,"personality":"custom persona","confidence":0.5}
---
# Soul

You are helpful and friendly.`

	traits := ParseSoulProfile(content)

	if traits.EnergyLevel != 0.8 {
		t.Errorf("front matter EnergyLevel: expected 0.8, got %f", traits.EnergyLevel)
	}
	if traits.Patience != 0.7 {
		t.Errorf("front matter Patience: expected 0.7, got %f", traits.Patience)
	}
	if traits.Verbosity != 0.6 {
		t.Errorf("front matter Verbosity: expected 0.6, got %f", traits.Verbosity)
	}
	if traits.Personality != "custom persona" {
		t.Errorf("front matter Personality: expected 'custom persona', got %q", traits.Personality)
	}
	if traits.Confidence != 0.5 {
		t.Errorf("front matter Confidence: expected 0.5, got %f", traits.Confidence)
	}
}

func TestParseSoulProfile_Combined(t *testing.T) {
	content := `---
profile: {"energy_level":0.6}
---
# Soul

You are an impatient and short-tempered bot.
topics: Go, Python`

	traits := ParseSoulProfile(content)

	// Front matter overrides keyword-based energy_level
	if traits.EnergyLevel != 0.6 {
		t.Errorf("expected EnergyLevel=0.6 from front matter, got %f", traits.EnergyLevel)
	}
	// Text-based mappings are NOT applied when front matter profile exists (early return)
	// So Patience stays at default 0.6
}

// ============================================================================
// MapProfileToEngagement 测试
// ============================================================================

func TestMapProfileToEngagement_HighEnergy(t *testing.T) {
	traits := BotProfileTraits{
		EnergyLevel: 1.0,
		Patience:    1.0,
		Verbosity:   1.0,
	}

	m := MapProfileToEngagement(traits)

	if m.ReplyProbability == nil || *m.ReplyProbability != 0.30 {
		t.Errorf("high energy → reply_probability=0.30, got %v", m.ReplyProbability)
	}
	if m.RateLimitCapacity == nil || *m.RateLimitCapacity != 10 {
		t.Errorf("high energy → rate_limit_capacity=10, got %v", m.RateLimitCapacity)
	}
	if m.BackoffBaseSeconds == nil || *m.BackoffBaseSeconds != 5.0 {
		t.Errorf("high patience → backoff_base_seconds=5.0, got %v", m.BackoffBaseSeconds)
	}
	if m.BackoffStartCount == nil || *m.BackoffStartCount != 5 {
		t.Errorf("high patience → backoff_start_count=5, got %v", m.BackoffStartCount)
	}
	// verbosity=1.0 → no MinLength/MaxLength set
	if m.MinLength != nil {
		t.Errorf("high verbosity should not set MinLength, got %v", m.MinLength)
	}
}

func TestMapProfileToEngagement_LowEnergyImpatient(t *testing.T) {
	traits := BotProfileTraits{
		EnergyLevel: 0.1,
		Patience:    0.1,
		Verbosity:   0.1,
	}

	m := MapProfileToEngagement(traits)

	if m.ReplyProbability == nil || *m.ReplyProbability < 0.05 || *m.ReplyProbability > 0.10 {
		t.Errorf("low energy → reply_probability ~0.075, got %v", m.ReplyProbability)
	}
	if m.BackoffBaseSeconds == nil || *m.BackoffBaseSeconds < 50 {
		t.Errorf("low patience → high backoff, got %v", m.BackoffBaseSeconds)
	}
	if m.BackoffStartCount == nil || *m.BackoffStartCount != 1 {
		t.Errorf("low patience → backoff_start_count=1, got %v", m.BackoffStartCount)
	}
	if m.MinLength == nil || *m.MinLength != 50 {
		t.Errorf("low verbosity → min_length=50, got %v", m.MinLength)
	}
	if m.MaxLength == nil || *m.MaxLength != 200 {
		t.Errorf("low verbosity → max_length=200, got %v", m.MaxLength)
	}
}

func TestMapProfileToEngagement_Keywords(t *testing.T) {
	traits := BotProfileTraits{
		PreferredTopics: []string{"Go", "Rust", "K8s"},
	}

	m := MapProfileToEngagement(traits)

	if len(m.Keywords) != 3 {
		t.Errorf("expected 3 keywords, got %d", len(m.Keywords))
	}
	if m.Keywords[0] != "Go" || m.Keywords[1] != "Rust" || m.Keywords[2] != "K8s" {
		t.Errorf("unexpected keywords: %v", m.Keywords)
	}
}

// ============================================================================
// mergeTraits 测试
// ============================================================================

func TestMergeTraits_OverwritesNonZero(t *testing.T) {
	dst := DefaultBotProfileTraits()
	src := BotProfileTraits{
		EnergyLevel: 0.8,
		Patience:    0.9,
		Confidence:  0.5,
	}

	mergeTraits(&dst, src)

	if dst.EnergyLevel != 0.8 {
		t.Errorf("EnergyLevel not merged: got %f", dst.EnergyLevel)
	}
	if dst.Patience != 0.9 {
		t.Errorf("Patience not merged: got %f", dst.Patience)
	}
	// Verbosity unchanged (src is 0 = zero value)
	if dst.Verbosity != 0.9 {
		t.Errorf("Verbosity should stay default: got %f", dst.Verbosity)
	}
	if dst.Confidence != 0.5 {
		t.Errorf("Confidence not merged: got %f", dst.Confidence)
	}
}

// ============================================================================
// DefaultBotProfileTraits 测试
// ============================================================================

func TestDefaultBotProfileTraits_Ranges(t *testing.T) {
	traits := DefaultBotProfileTraits()
	if traits.EnergyLevel < 0 || traits.EnergyLevel > 1 {
		t.Errorf("EnergyLevel out of range: %f", traits.EnergyLevel)
	}
	if traits.Patience < 0 || traits.Patience > 1 {
		t.Errorf("Patience out of range: %f", traits.Patience)
	}
	if traits.Verbosity < 0 || traits.Verbosity > 1 {
		t.Errorf("Verbosity out of range: %f", traits.Verbosity)
	}
	if traits.Confidence < 0 || traits.Confidence > 1 {
		t.Errorf("Confidence out of range: %f", traits.Confidence)
	}
}
