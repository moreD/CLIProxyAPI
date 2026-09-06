package billing

import (
	"math"
	"regexp"
	"strings"
)

// Rate data migrated from the client-usage panel. OpenAI rates verified 2026-09-05:
// https://developers.openai.com/api/docs/pricing
// Unknown models use GPT-6 Astra, including its context and service-tier rules.
type modelRate struct {
	pattern              *regexp.Regexp
	input, output, cache float64
}

func knownRatePattern(pattern string) *regexp.Regexp {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "(?i)"), "$")
	return regexp.MustCompile("(?i)^(?:" + pattern + ")(?:-[0-9]{8}|-[0-9]{4}-[0-9]{2}-[0-9]{2})?$")
}

var modelRates = []modelRate{
	{knownRatePattern("(?i)claude.*(fable|mythos).*5"), 10, 50, 1},
	{knownRatePattern("(?i)claude.*opus.*4\\.[5-8]"), 5, 25, 0.5},
	{knownRatePattern("(?i)claude.*opus.*4(\\.1)?"), 15, 75, 1.5},
	{knownRatePattern("(?i)claude.*sonnet.*4(\\.[56])?"), 3, 15, 0.3},
	{knownRatePattern("(?i)claude.*haiku.*4\\.5"), 1, 5, 0.1},
	{knownRatePattern("(?i)claude.*haiku.*3\\.5"), 0.8, 4, 0.08},
	{knownRatePattern("(?i)claude.*opus"), 5, 25, 0.5},
	{knownRatePattern("(?i)claude.*sonnet"), 3, 15, 0.3},
	{knownRatePattern("(?i)claude.*haiku"), 1, 5, 0.1},
	{knownRatePattern("(?i)gpt-6-astra"), 10, 50, 1},
	{knownRatePattern("(?i)gpt-5\\.6-sol"), 4, 20, 0.4},
	{knownRatePattern("(?i)gpt-5\\.6-terra"), 2, 12, 0.2},
	{knownRatePattern("(?i)gpt-5\\.6-luna"), 0.2, 1.2, 0.02},
	{knownRatePattern("(?i)gpt-5\\.5-pro"), 30, 180, 30},
	{knownRatePattern("(?i)gpt-5\\.5"), 5, 30, 0.5},
	{knownRatePattern("(?i)gpt-5\\.4-pro"), 30, 180, 30},
	{knownRatePattern("(?i)gpt-5\\.4-mini"), 0.75, 4.5, 0.075},
	{knownRatePattern("(?i)gpt-5\\.4-nano"), 0.2, 1.25, 0.02},
	{knownRatePattern("(?i)gpt-5\\.4"), 2.5, 15, 0.25},
	{knownRatePattern("(?i)gpt-5\\.3-(chat-latest|codex)"), 1.75, 14, 0.175},
	{knownRatePattern("(?i)gpt-5\\.2-(chat-latest|codex)"), 1.75, 14, 0.175},
	{knownRatePattern("(?i)gpt-5\\.2-pro"), 21, 168, 21},
	{knownRatePattern("(?i)gpt-5\\.2"), 1.75, 14, 0.175},
	{knownRatePattern("(?i)gpt-5\\.1-codex-mini"), 0.25, 2, 0.025},
	{knownRatePattern("(?i)gpt-5\\.1-(chat-latest|codex(-max)?)"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gpt-5\\.1"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gpt-5-codex"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gpt-5-chat-latest"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gpt-5-pro"), 15, 120, 15},
	{knownRatePattern("(?i)gpt-5-mini"), 0.25, 2, 0.025},
	{knownRatePattern("(?i)gpt-5-nano"), 0.05, 0.4, 0.005},
	{knownRatePattern("(?i)gpt-5$"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gpt-4\\.1-mini"), 0.4, 1.6, 0.1},
	{knownRatePattern("(?i)gpt-4\\.1-nano"), 0.1, 0.4, 0.025},
	{knownRatePattern("(?i)gpt-4\\.1"), 2, 8, 0.5},
	{knownRatePattern("(?i)gpt-4o-mini"), 0.15, 0.6, 0.075},
	{knownRatePattern("(?i)gpt-4o-2024-05-13"), 5, 15, 5},
	{knownRatePattern("(?i)gpt-4o"), 2.5, 10, 1.25},
	{knownRatePattern("(?i)o4-mini"), 1.1, 4.4, 0.275},
	{knownRatePattern("(?i)o3-pro"), 20, 80, 20},
	{knownRatePattern("(?i)o3-mini"), 1.1, 4.4, 0.55},
	{knownRatePattern("(?i)o3"), 2, 8, 0.5},
	{knownRatePattern("(?i)o1-pro"), 150, 600, 150},
	{knownRatePattern("(?i)o1"), 15, 60, 7.5},
	{knownRatePattern("(?i)gemini-3\\.5-flash"), 1.5, 9, 0.15},
	{knownRatePattern("(?i)gemini-3(\\.1)?-pro"), 2, 12, 0.2},
	{knownRatePattern("(?i)gemini-3\\.1-flash-lite"), 0.25, 1.5, 0.025},
	{knownRatePattern("(?i)gemini-2\\.5-pro"), 1.25, 10, 0.125},
	{knownRatePattern("(?i)gemini-2\\.5-flash-lite"), 0.1, 0.4, 0.01},
	{knownRatePattern("(?i)gemini-2\\.5-flash"), 0.3, 2.5, 0.03},
	{knownRatePattern("(?i)kimi.*k2"), 0.15, 2.5, 0.15},
}

type Tokens struct{ Input, Output, CacheRead, CacheWrite int64 }

var modelSnapshotSuffix = regexp.MustCompile(`-[0-9]{8}$|-[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func Price(model, tier string, tokens Tokens, longContext bool) USD {
	slug := strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(slug, "/"); index >= 0 {
		slug = slug[index+1:]
	}
	slug = modelSnapshotSuffix.ReplaceAllString(slug, "")
	rate := modelRate{input: 10, output: 50, cache: 1}
	found := false
	for _, candidate := range modelRates {
		if candidate.pattern.MatchString(slug) {
			rate = candidate
			found = true
			break
		}
	}
	modern := !found || strings.HasPrefix(slug, "gpt-6-astra") || strings.HasPrefix(slug, "gpt-5.6-")
	creation := rate.input
	if modern || strings.Contains(slug, "claude") {
		creation *= 1.25
	}
	input := max(tokens.Input, 0)
	read := min(max(tokens.CacheRead, 0), input)
	write := min(max(tokens.CacheWrite, 0), input-read)
	prompt := input - read - write
	inMultiplier, outMultiplier := 1.0, 1.0
	legacyLong := slug == "gpt-5.5" || slug == "gpt-5.4" || slug == "gpt-5.4-pro" || slug == "gpt-5.5-pro"
	if longContext && input > 272_000 && (modern || legacyLong) {
		inMultiplier = 2
		outMultiplier = 1.5
	}
	multiplier := 1.0
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "batch", "flex":
		multiplier = 0.5
	case "fast", "priority":
		if modern {
			multiplier = 2
		} else if !(longContext && input > 272_000 && legacyLong) {
			switch {
			case strings.HasPrefix(slug, "gpt-5.5"):
				multiplier = 2.5
			case strings.HasPrefix(slug, "gpt-5.4"), strings.HasPrefix(slug, "gpt-5.3-codex"), strings.HasPrefix(slug, "gpt-5.2"), strings.HasPrefix(slug, "gpt-5.1"):
				multiplier = 2
			}
		}
	}
	// Integer nanodollars avoid floating-point drift when accumulating requests.
	nanos := ((float64(prompt)*rate.input+float64(read)*rate.cache+float64(write)*creation)*inMultiplier +
		float64(max(tokens.Output, 0))*rate.output*outMultiplier) * multiplier * 1000
	if nanos >= float64(int64(1<<63-1)) {
		return USD(1<<63 - 1)
	}
	return USD(math.Round(nanos))
}
