package billing

import "testing"

func TestPriceAstraSolAndUnknownFallback(t *testing.T) {
	tests := []struct {
		model string
		want  USD
	}{
		{model: "gpt-6-astra", want: USD(10_000_000_000)},
		{model: "provider/gpt-5.6-sol", want: USD(4_000_000_000)},
		{model: "future-unknown-model", want: USD(10_000_000_000)},
		{model: "future-gpt-5-mini-v2", want: USD(10_000_000_000)},
		{model: "gpt-5-mini-v2", want: USD(10_000_000_000)},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := Price(tt.model, "standard", Tokens{Input: 1_000_000}, false); got != tt.want {
				t.Fatalf("Price() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPriceUsesCanonicalOutputIncludingReasoningOnce(t *testing.T) {
	withReasoningIncluded := Price("gpt-5.6-sol", "standard", Tokens{Output: 125}, false)
	if withReasoningIncluded != USD(2_500_000) {
		t.Fatalf("125 canonical output tokens cost = %s, want 0.002500000", withReasoningIncluded)
	}
}

func TestPriceServiceTiersAndLongContext(t *testing.T) {
	tokens := Tokens{Input: 273_000, Output: 10_000}
	if got := Price("gpt-6-astra", "standard", tokens, true); got != USD(6_210_000_000) {
		t.Fatalf("long-context standard = %s, want 6.210000000", got)
	}
	if got := Price("gpt-6-astra", "priority", tokens, true); got != USD(12_420_000_000) {
		t.Fatalf("long-context priority = %s, want 12.420000000", got)
	}
	if got := Price("gpt-6-astra", "batch", tokens, false); got != USD(1_615_000_000) {
		t.Fatalf("batch = %s, want 1.615000000", got)
	}
}
