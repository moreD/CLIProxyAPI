// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
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
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

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
	Name        string            `yaml:"name,omitempty" json:"name,omitempty"`
	APIKey      string            `yaml:"api-key" json:"api-key"`
	TokenLimits APIKeyTokenLimits `yaml:"token-limits,omitempty" json:"token-limits,omitempty"`
}

// APIKeyTokenLimits defines optional per-client API key token limits.
type APIKeyTokenLimits struct {
	TwelveHour int64 `yaml:"12h,omitempty" json:"12h,omitempty"`
	SevenDay   int64 `yaml:"7d,omitempty" json:"7d,omitempty"`
}

func (l APIKeyTokenLimits) IsZero() bool {
	return l.TwelveHour <= 0 && l.SevenDay <= 0
}

func (e *APIKeyEntry) UnmarshalYAML(value *yaml.Node) error {
	if e == nil || value == nil {
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		e.APIKey = strings.TrimSpace(value.Value)
		e.Name = ""
		e.TokenLimits = APIKeyTokenLimits{}
		return nil
	}
	type rawAPIKeyEntry APIKeyEntry
	var raw rawAPIKeyEntry
	if err := value.Decode(&raw); err != nil {
		return err
	}
	e.Name = strings.TrimSpace(raw.Name)
	e.APIKey = strings.TrimSpace(raw.APIKey)
	e.TokenLimits = normalizeAPIKeyTokenLimits(raw.TokenLimits)
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
		e.TokenLimits = APIKeyTokenLimits{}
		return nil
	}
	type rawAPIKeyEntry APIKeyEntry
	var raw rawAPIKeyEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	e.Name = strings.TrimSpace(raw.Name)
	e.APIKey = strings.TrimSpace(raw.APIKey)
	e.TokenLimits = normalizeAPIKeyTokenLimits(raw.TokenLimits)
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
			Name:        strings.TrimSpace(entry.Name),
			APIKey:      apiKey,
			TokenLimits: normalizeAPIKeyTokenLimits(entry.TokenLimits),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAPIKeyTokenLimits(limits APIKeyTokenLimits) APIKeyTokenLimits {
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

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
