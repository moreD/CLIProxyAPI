package billing

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	ReplaceCatalog(map[string]PriceRate{
		"gpt-6-astra": {
			Input: 10, Output: 50, CacheRead: 1,
			ContextTiers: []PriceContextRate{{Threshold: 200001, Input: 20, Output: 75, CacheRead: 2}},
			ServiceTiers: map[string]PriceRate{"priority": {Input: 30, Output: 100, CacheRead: 3}},
		},
		"gpt-5.6-sol": {Input: 4, Output: 20, CacheRead: 0.4},
	})
	os.Exit(m.Run())
}

func TestPriceUsesSynchronizedCatalogAndUnknownIsUnpriced(t *testing.T) {
	if got := Price("gpt-6-astra", "standard", Tokens{Input: 1_000_000}, false); got != USD(10_000_000_000) {
		t.Fatalf("catalog price = %s, want 10.000000000", got)
	}
	if got := Price("provider/gpt-5.6-sol", "standard", Tokens{Input: 1_000_000}, false); got != USD(4_000_000_000) {
		t.Fatalf("provider alias price = %s, want 4.000000000", got)
	}
	if got := Price("future-unknown-model", "standard", Tokens{Input: 1_000_000}, false); got != 0 {
		t.Fatalf("unknown model price = %s, want zero", got)
	}
}

func TestPriceUsesCanonicalOutputIncludingReasoningOnce(t *testing.T) {
	if got := Price("gpt-5.6-sol", "standard", Tokens{Output: 125}, false); got != USD(2_500_000) {
		t.Fatalf("125 canonical output tokens cost = %s, want 0.002500000", got)
	}
}

func TestPriceUsesServiceAndContextTiers(t *testing.T) {
	tokens := Tokens{Input: 273_000, Output: 10_000}
	if got := Price("gpt-6-astra", "standard", tokens, true); got != USD(6_210_000_000) {
		t.Fatalf("long-context standard = %s, want 6.210000000", got)
	}
	if got := Price("gpt-6-astra", "priority", tokens, true); got != USD(6_210_000_000) {
		t.Fatalf("long-context priority = %s, want 6.210000000", got)
	}
}

func TestParsePriceSources(t *testing.T) {
	modelsDev, err := parseModelsDevCatalog([]byte(`{"models":{"openai/gpt-test":{"id":"openai/gpt-test"}},"providers":{"openai":{"models":{"gpt-test":{"cost":{"input":1,"output":2,"cache_read":0.1,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":200000}}]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":5},"provider":{"body":{"service_tier":"priority"}}}}}},"fast-only":{"cost":{"input":6,"output":7},"experimental":{"modes":{"fast":{"cost":{"input":8,"output":9}}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rate, ok := modelsDev["gpt-test"]
	if !ok || rate.input != 1 || len(rate.contextTiers) != 1 || rate.serviceTiers["priority"].output != 5 {
		t.Fatalf("models.dev rate = %#v", rate)
	}
	if got := modelsDev["fast-only"].serviceTiers["priority"].output; got != 9 {
		t.Fatalf("models.dev fast mode priority rate = %v, want 9", got)
	}

	lite, err := parseLiteLLMCatalog([]byte(`{"sample_spec":{},"gpt-test":{"input_cost_per_token":"0.000001","output_cost_per_token":0.000002,"input_cache_read":0.0000001,"input_cache_write":0.0000002}}`))
	if err != nil || lite["gpt-test"].input != 1 || lite["gpt-test"].output != 2 || lite["gpt-test"].cacheRead < 0.099 || lite["gpt-test"].cacheRead > 0.101 {
		t.Fatalf("LiteLLM rates = %#v, err=%v", lite, err)
	}

	openRouter, err := parseOpenRouterCatalog([]byte(`{"data":[{"id":"provider/gpt-test","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001","input_cache_write":"0.0000002"}}]}`))
	if err != nil || openRouter["provider/gpt-test"].input != 1 || openRouter["gpt-test"].output != 2 || openRouter["provider/gpt-test"].cacheRead < 0.099 || openRouter["provider/gpt-test"].cacheWrite < 0.199 {
		t.Fatalf("OpenRouter rates = %#v, err=%v", openRouter, err)
	}
}
