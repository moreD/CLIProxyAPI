// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"strings"

	"gopkg.in/yaml.v3"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// CodexResponseSteering mirrors the provider-wide runtime setting for API handlers.
	CodexResponseSteering bool `yaml:"-" json:"-"`

	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of named keys for authenticating clients to this proxy server.
	APIKeys []APIKeyEntry `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// APIKeyEntry is a client API key plus an optional operator-facing display name.
type APIKeyEntry struct {
	Name       string           `yaml:"name,omitempty" json:"name,omitempty"`
	APIKey     string           `yaml:"api-key" json:"api-key"`
	CostLimits APIKeyCostLimits `yaml:"cost-limits,omitempty" json:"cost-limits,omitempty"`
}

// APIKeyCostLimits defines optional per-client API key limits in USD.
type APIKeyCostLimits struct {
	TwelveHour billing.USD `yaml:"12h,omitempty" json:"12h,omitempty"`
	SevenDay   billing.USD `yaml:"7d,omitempty" json:"7d,omitempty"`
}

func (l APIKeyCostLimits) IsZero() bool {
	return l.TwelveHour <= 0 && l.SevenDay <= 0
}

func (e *APIKeyEntry) UnmarshalYAML(value *yaml.Node) error {
	if e == nil || value == nil {
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		e.APIKey = strings.TrimSpace(value.Value)
		e.Name = ""
		e.CostLimits = APIKeyCostLimits{}
		return nil
	}
	type rawAPIKeyEntry APIKeyEntry
	var raw rawAPIKeyEntry
	if err := value.Decode(&raw); err != nil {
		return err
	}
	var legacy struct {
		Limits map[string]int64 `yaml:"token-limits"`
	}
	if err := value.Decode(&legacy); err != nil {
		return err
	}
	hasTokenLimits := false
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == "cost-limits" {
			hasTokenLimits = true
		}
	}
	if !hasTokenLimits {
		raw.CostLimits = APIKeyCostLimits{TwelveHour: billing.LegacyLimit(legacy.Limits["12h"]), SevenDay: billing.LegacyLimit(legacy.Limits["7d"])}
	}
	e.Name = strings.TrimSpace(raw.Name)
	e.APIKey = strings.TrimSpace(raw.APIKey)
	e.CostLimits = normalizeAPIKeyCostLimits(raw.CostLimits)
	return nil
}

func (e *APIKeyEntry) UnmarshalJSON(data []byte) error {
	if e == nil {
		return nil
	}
	var key string
	if err := json.Unmarshal(data, &key); err == nil {
		e.APIKey = strings.TrimSpace(key)
		e.Name = ""
		e.CostLimits = APIKeyCostLimits{}
		return nil
	}
	type rawAPIKeyEntry APIKeyEntry
	var raw rawAPIKeyEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["cost-limits"]; !present {
		var legacy map[string]int64
		if value, ok := fields["token-limits"]; ok {
			if err := json.Unmarshal(value, &legacy); err != nil {
				return err
			}
			raw.CostLimits = APIKeyCostLimits{TwelveHour: billing.LegacyLimit(legacy["12h"]), SevenDay: billing.LegacyLimit(legacy["7d"])}
		}
	}
	e.Name = strings.TrimSpace(raw.Name)
	e.APIKey = strings.TrimSpace(raw.APIKey)
	e.CostLimits = normalizeAPIKeyCostLimits(raw.CostLimits)
	return nil
}

func NormalizeAPIKeyEntries(entries []APIKeyEntry) []APIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]APIKeyEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		apiKey := strings.TrimSpace(entry.APIKey)
		if apiKey == "" {
			continue
		}
		if _, ok := seen[apiKey]; ok {
			continue
		}
		seen[apiKey] = struct{}{}
		out = append(out, APIKeyEntry{
			Name:       strings.TrimSpace(entry.Name),
			APIKey:     apiKey,
			CostLimits: normalizeAPIKeyCostLimits(entry.CostLimits),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAPIKeyCostLimits(limits APIKeyCostLimits) APIKeyCostLimits {
	if limits.TwelveHour < 0 {
		limits.TwelveHour = 0
	}
	if limits.SevenDay < 0 {
		limits.SevenDay = 0
	}
	return limits
}

func APIKeyValues(entries []APIKeyEntry) []string {
	entries = NormalizeAPIKeyEntries(entries)
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.APIKey)
	}
	return out
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
