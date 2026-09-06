package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

func TestPrepareConfigConvertsLegacyLimitsAndPreservesOtherYAML(t *testing.T) {
	path := writeConfigTestFile(t, `port: 8317
debug: true
api-keys:
  - name: legacy
    api-key: secret
    token-limits:
      12h: 1000000
      7d: 2000000
providers:
  custom:
    enabled: true
`)
	data, changed, err := prepareConfig(path)
	if err != nil || !changed {
		t.Fatalf("prepareConfig() changed=%v err=%v", changed, err)
	}
	var doc struct {
		Port      int                  `yaml:"port"`
		Debug     bool                 `yaml:"debug"`
		APIKeys   []config.APIKeyEntry `yaml:"api-keys"`
		Providers map[string]struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"providers"`
	}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Port != 8317 || !doc.Debug || !doc.Providers["custom"].Enabled {
		t.Fatalf("unrelated configuration changed: %+v", doc)
	}
	if got := doc.APIKeys[0].CostLimits; got.TwelveHour.String() != "0.400000000" || got.SevenDay.String() != "0.800000000" {
		t.Fatalf("converted limits = %+v", got)
	}
	if strings.Contains(string(data), "token-limits:") {
		t.Fatalf("legacy field remains:\n%s", data)
	}

	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	second, changedAgain, err := prepareConfig(path)
	if err != nil || changedAgain || string(second) != string(data) {
		t.Fatalf("second pass changed=%v err=%v\nfirst: %s\nsecond: %s", changedAgain, err, data, second)
	}
}

func TestPrepareConfigExplicitZeroCostLimitsTakePrecedence(t *testing.T) {
	path := writeConfigTestFile(t, `api-keys:
  - api-key: explicit
    cost-limits:
      12h: 0
      7d: 0
    token-limits:
      12h: 1000000
`)
	data, changed, err := prepareConfig(path)
	if err != nil || !changed {
		t.Fatalf("prepareConfig() changed=%v err=%v", changed, err)
	}
	var doc struct {
		APIKeys []config.APIKeyEntry `yaml:"api-keys"`
	}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.APIKeys[0].CostLimits.IsZero() || strings.Contains(string(data), "token-limits:") {
		t.Fatalf("explicit zero did not take precedence:\n%s", data)
	}
}

func writeConfigTestFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
