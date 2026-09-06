package config

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"gopkg.in/yaml.v3"
)

func TestAPIKeyEntryConvertsLegacyTokenLimits(t *testing.T) {
	for name, data := range map[string][]byte{
		"yaml": []byte("api-key: legacy\ntoken-limits:\n  12h: 1000000\n  7d: 2000000\n"),
		"json": []byte(`{"api-key":"legacy","token-limits":{"12h":1000000,"7d":2000000}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var got APIKeyEntry
			var err error
			if name == "yaml" {
				err = yaml.Unmarshal(data, &got)
			} else {
				err = json.Unmarshal(data, &got)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.CostLimits.TwelveHour != billing.USD(400_000_000) || got.CostLimits.SevenDay != billing.USD(800_000_000) {
				t.Fatalf("cost limits = %+v", got.CostLimits)
			}
		})
	}
}

func TestAPIKeyEntryExplicitZeroCostLimitsOverrideLegacy(t *testing.T) {
	for name, data := range map[string][]byte{
		"yaml": []byte("api-key: explicit\ncost-limits:\n  12h: 0\n  7d: 0\ntoken-limits:\n  12h: 1000000\n"),
		"json": []byte(`{"api-key":"explicit","cost-limits":{"12h":0,"7d":0},"token-limits":{"12h":1000000}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var got APIKeyEntry
			var err error
			if name == "yaml" {
				err = yaml.Unmarshal(data, &got)
			} else {
				err = json.Unmarshal(data, &got)
			}
			if err != nil || !got.CostLimits.IsZero() {
				t.Fatalf("entry = %+v, err = %v; want explicit zero limits", got, err)
			}
		})
	}
}
