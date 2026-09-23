package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	modelsDevPriceURL    = "https://models.dev/catalog.json"
	liteLLMPriceURL      = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	openRouterPriceURL   = "https://openrouter.ai/api/v1/models"
	priceRefreshInterval = time.Hour
	priceSourceTimeout   = 10 * time.Second
)

// Tokens contains the canonical token buckets used for billing.
type Tokens struct{ Input, Output, CacheRead, CacheWrite int64 }

// PriceRate expresses USD per million tokens for a model.
// A matching context tier takes precedence over a service tier, matching the
// pricing rules used by CPA Manager Plus.
type PriceRate struct {
	Input, Output, CacheRead, CacheWrite float64
	ContextTiers                         []PriceContextRate
	ServiceTiers                         map[string]PriceRate
}

type PriceContextRate struct {
	Threshold  int64
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

type modelRate struct {
	input, output, cacheRead, cacheWrite float64
	contextTiers                         []contextRate
	serviceTiers                         map[string]modelRate
}

type contextRate struct {
	threshold int64
	rate      modelRate
}

type priceCatalog struct {
	rates map[string]modelRate
}

var activeCatalog atomic.Value // *priceCatalog

func init() {
	activeCatalog.Store(&priceCatalog{rates: map[string]modelRate{}})
}

// ReplaceCatalog replaces the active in-memory pricing catalog.
// The runtime refresh loop uses this same operation after each successful sync.
func ReplaceCatalog(rates map[string]PriceRate) {
	catalog := make(map[string]modelRate, len(rates))
	for model, publicRate := range rates {
		rate := modelRate{
			input: publicRate.Input, output: publicRate.Output,
			cacheRead: publicRate.CacheRead, cacheWrite: publicRate.CacheWrite,
			serviceTiers: make(map[string]modelRate, len(publicRate.ServiceTiers)),
		}
		for _, tier := range publicRate.ContextTiers {
			rate.contextTiers = append(rate.contextTiers, contextRate{
				threshold: tier.Threshold,
				rate:      modelRate{input: tier.Input, output: tier.Output, cacheRead: tier.CacheRead, cacheWrite: tier.CacheWrite},
			})
		}
		sort.SliceStable(rate.contextTiers, func(i, j int) bool {
			return rate.contextTiers[i].threshold < rate.contextTiers[j].threshold
		})
		for name, tier := range publicRate.ServiceTiers {
			rate.serviceTiers[strings.ToLower(strings.TrimSpace(name))] = modelRate{
				input: tier.Input, output: tier.Output, cacheRead: tier.CacheRead, cacheWrite: tier.CacheWrite,
			}
		}
		catalog[normalizeModelID(model)] = rate
	}
	activeCatalog.Store(&priceCatalog{rates: catalog})
}

// StartPriceRefresh starts the initial price load and the hourly refresh loop.
// The first refresh runs asynchronously so service startup remains responsive.
func StartPriceRefresh(ctx context.Context, proxyURL string) {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		refreshPrices(ctx, proxyURL)
		ticker := time.NewTicker(priceRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshPrices(ctx, proxyURL)
			}
		}
	}()
}

func refreshPrices(parent context.Context, proxyURL string) {
	ctx, cancel := context.WithTimeout(parent, priceSourceTimeout*3)
	defer cancel()
	catalog, sources, errRefresh := loadPriceCatalog(ctx, proxyURL)
	if errRefresh != nil {
		log.Warnf("model price refresh failed: %v", errRefresh)
		return
	}
	activeCatalog.Store(catalog)
	log.Infof("model price refresh completed: %d models from %s", len(catalog.rates), strings.Join(sources, ", "))
}

func loadPriceCatalog(ctx context.Context, proxyURL string) (*priceCatalog, []string, error) {
	client := &http.Client{Timeout: priceSourceTimeout}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyURL); errProxy != nil {
		return nil, nil, fmt.Errorf("build price refresh transport: %w", errProxy)
	} else if transport != nil {
		client.Transport = transport
	}

	merged := make(map[string]modelRate)
	sources := make([]string, 0, 3)
	var errs []string
	for _, source := range []struct {
		name string
		url  string
		load func([]byte) (map[string]modelRate, error)
	}{
		{name: "models.dev", url: modelsDevPriceURL, load: parseModelsDevCatalog},
		{name: "litellm", url: liteLLMPriceURL, load: parseLiteLLMCatalog},
		{name: "openrouter", url: openRouterPriceURL, load: parseOpenRouterCatalog},
	} {
		body, errFetch := fetchPriceSource(ctx, client, source.url)
		if errFetch != nil {
			errs = append(errs, source.name+": "+errFetch.Error())
			continue
		}
		rates, errParse := source.load(body)
		if errParse != nil {
			errs = append(errs, source.name+": "+errParse.Error())
			continue
		}
		added := 0
		for model, rate := range rates {
			if _, exists := merged[model]; exists {
				continue
			}
			merged[model] = rate
			added++
		}
		if added > 0 {
			sources = append(sources, source.name)
		}
	}
	if len(merged) == 0 {
		if len(errs) == 0 {
			return nil, nil, errors.New("all price sources returned no usable models")
		}
		return nil, nil, errors.New(strings.Join(errs, "; "))
	}
	return &priceCatalog{rates: merged}, sources, nil
}

func fetchPriceSource(ctx context.Context, client *http.Client, sourceURL string) ([]byte, error) {
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI model price updater")
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, errDo
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("status %s", response.Status)
	}
	decoder := json.NewDecoder(response.Body)
	var payload json.RawMessage
	if errDecode := decoder.Decode(&payload); errDecode != nil {
		return nil, errDecode
	}
	return payload, nil
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Cost         modelsDevCost         `json:"cost"`
	Experimental modelsDevExperimental `json:"experimental"`
}

type modelsDevCost struct {
	Input           float64         `json:"input"`
	Output          float64         `json:"output"`
	CacheRead       float64         `json:"cache_read"`
	CacheWrite      float64         `json:"cache_write"`
	Tiers           []modelsDevTier `json:"tiers"`
	ContextOver200k *modelsDevCost  `json:"context_over_200k"`
}

type modelsDevTier struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

type modelsDevExperimental struct {
	Modes map[string]modelsDevMode `json:"modes"`
}

type modelsDevMode struct {
	Cost     modelsDevCost `json:"cost"`
	Provider struct {
		Body struct {
			ServiceTier string `json:"service_tier"`
		} `json:"body"`
	} `json:"provider"`
}

func parseModelsDevCatalog(body []byte) (map[string]modelRate, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	providerPayload := root
	if rawProviders, ok := root["providers"]; ok {
		if err := json.Unmarshal(rawProviders, &providerPayload); err != nil {
			return nil, err
		}
	}
	rates := make(map[string]modelRate)
	for provider, rawEntry := range providerPayload {
		var entry modelsDevProvider
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			return nil, err
		}
		for model, definition := range entry.Models {
			rate := rateFromModelsDevCost(definition.Cost)
			if !rateUsable(rate) {
				continue
			}
			if len(definition.Experimental.Modes) > 0 {
				rate.serviceTiers = make(map[string]modelRate)
				for mode, experimental := range definition.Experimental.Modes {
					modeRate := rateFromModelsDevCost(experimental.Cost)
					if !rateUsable(modeRate) {
						continue
					}
					modeName := strings.ToLower(strings.TrimSpace(mode))
					serviceTier := strings.ToLower(strings.TrimSpace(experimental.Provider.Body.ServiceTier))
					if modeName != "" {
						rate.serviceTiers[modeName] = modeRate
					}
					if serviceTier != "" {
						rate.serviceTiers[serviceTier] = modeRate
					}
					if modeName == "fast" {
						rate.serviceTiers["priority"] = modeRate
					}
				}
			}
			rates[normalizeModelID(provider+"/"+model)] = rate
		}
	}
	return addUnambiguousAliases(rates), nil
}

func rateFromModelsDevCost(cost modelsDevCost) modelRate {
	rate := modelRate{input: cost.Input, output: cost.Output, cacheRead: cost.CacheRead, cacheWrite: cost.CacheWrite}
	if cost.ContextOver200k != nil {
		rate.contextTiers = append(rate.contextTiers, contextRate{threshold: 200000, rate: rateFromModelsDevCost(*cost.ContextOver200k)})
	}
	for _, tier := range cost.Tiers {
		if strings.EqualFold(strings.TrimSpace(tier.Tier.Type), "context") && tier.Tier.Size > 0 {
			rate.contextTiers = append(rate.contextTiers, contextRate{threshold: tier.Tier.Size, rate: modelRate{input: tier.Input, output: tier.Output, cacheRead: tier.CacheRead, cacheWrite: tier.CacheWrite}})
		}
	}
	sort.SliceStable(rate.contextTiers, func(i, j int) bool {
		return rate.contextTiers[i].threshold < rate.contextTiers[j].threshold
	})
	return rate
}

func parseLiteLLMCatalog(body []byte) (map[string]modelRate, error) {
	var rawEntries map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawEntries); err != nil {
		return nil, err
	}
	rates := make(map[string]modelRate, len(rawEntries))
	for model, rawEntry := range rawEntries {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}
		rate := modelRate{
			input:      jsonNumber(entry["input_cost_per_token"]) * 1_000_000,
			output:     jsonNumber(entry["output_cost_per_token"]) * 1_000_000,
			cacheRead:  jsonNumberFirst(entry, "cache_read_input_token_cost", "input_cache_read") * 1_000_000,
			cacheWrite: jsonNumberFirst(entry, "cache_creation_input_token_cost", "cache_write_input_token_cost", "input_cache_write", "input_cache_creation") * 1_000_000,
		}
		if rateUsable(rate) {
			rates[normalizeModelID(model)] = rate
		}
	}
	return addUnambiguousAliases(rates), nil
}

type openRouterCatalog struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID      string                     `json:"id"`
	Pricing map[string]json.RawMessage `json:"pricing"`
}

func parseOpenRouterCatalog(body []byte) (map[string]modelRate, error) {
	var catalog openRouterCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}
	rates := make(map[string]modelRate, len(catalog.Data))
	for _, model := range catalog.Data {
		rate := modelRate{
			input:      jsonNumber(model.Pricing["prompt"]) * 1_000_000,
			output:     jsonNumber(model.Pricing["completion"]) * 1_000_000,
			cacheRead:  jsonNumberFirst(model.Pricing, "input_cache_read", "cache_read_input_token_cost", "cache_read") * 1_000_000,
			cacheWrite: jsonNumberFirst(model.Pricing, "input_cache_write", "input_cache_creation", "cache_creation_input_token_cost", "cache_write_input_token_cost", "cache_write") * 1_000_000,
		}
		if strings.TrimSpace(model.ID) != "" && rateUsable(rate) {
			rates[normalizeModelID(model.ID)] = rate
		}
	}
	return addUnambiguousAliases(rates), nil
}

func jsonNumber(raw json.RawMessage) float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		_, _ = fmt.Sscan(strings.TrimSpace(text), &number)
	}
	return number
}

func jsonNumberFirst(values map[string]json.RawMessage, keys ...string) float64 {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			return jsonNumber(raw)
		}
	}
	return 0
}

func rateUsable(rate modelRate) bool {
	return rate.input > 0 || rate.output > 0 || rate.cacheRead > 0 || rate.cacheWrite > 0
}

func normalizeModelID(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func addUnambiguousAliases(rates map[string]modelRate) map[string]modelRate {
	aliases := make(map[string]string)
	for model := range rates {
		_, tail, hasPrefix := strings.Cut(model, "/")
		if !hasPrefix || tail == "" {
			continue
		}
		if existing, ok := aliases[tail]; ok && existing != model {
			aliases[tail] = ""
			continue
		}
		aliases[tail] = model
	}
	for alias, model := range aliases {
		if model != "" {
			rates[alias] = rates[model]
		}
	}
	return rates
}

// Price calculates a request price from the latest synchronized catalog.
// Unpriced models return zero until a source provides a usable price.
func Price(model, tier string, tokens Tokens, longContext bool) USD {
	fullID := normalizeModelID(model)
	slug := fullID
	if index := strings.LastIndex(slug, "/"); index >= 0 {
		slug = slug[index+1:]
	}
	catalog := activeCatalog.Load().(*priceCatalog)
	rate, ok := catalog.rates[fullID]
	if !ok {
		rate, ok = catalog.rates[slug]
	}
	if !ok {
		return 0
	}
	contextTierApplied := false
	if longContext && len(rate.contextTiers) > 0 {
		input := max(tokens.Input, 0)
		for _, contextTier := range rate.contextTiers {
			if input > contextTier.threshold {
				rate.input, rate.output, rate.cacheRead, rate.cacheWrite = contextTier.rate.input, contextTier.rate.output, contextTier.rate.cacheRead, contextTier.rate.cacheWrite
				contextTierApplied = true
			}
		}
	}
	if !contextTierApplied {
		if serviceRate, exists := rate.serviceTiers[strings.ToLower(strings.TrimSpace(tier))]; exists {
			rate = serviceRate
		}
	}
	input := max(tokens.Input, 0)
	read := min(max(tokens.CacheRead, 0), input)
	write := min(max(tokens.CacheWrite, 0), input-read)
	prompt := input - read - write
	nanos := (float64(prompt)*rate.input + float64(read)*rate.cacheRead + float64(write)*rate.cacheWrite + float64(max(tokens.Output, 0))*rate.output) * 1000
	if nanos >= float64(int64(1<<63-1)) {
		return USD(1<<63 - 1)
	}
	return USD(math.Round(nanos))
}
