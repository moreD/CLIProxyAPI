package billing

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUSDExactJSONAndYAMLDollars(t *testing.T) {
	const want USD = 1_234_567_891
	for name, data := range map[string][]byte{
		"json": []byte(`1.234567891`),
		"yaml": []byte("1.234567891\n"),
	} {
		t.Run(name, func(t *testing.T) {
			var got USD
			var err error
			if name == "json" {
				err = json.Unmarshal(data, &got)
			} else {
				err = yaml.Unmarshal(data, &got)
			}
			if err != nil || got != want {
				t.Fatalf("decode = %s, %v; want %s", got, err, want)
			}
			var encoded []byte
			if name == "json" {
				encoded, err = json.Marshal(got)
			} else {
				encoded, err = yaml.Marshal(got)
			}
			if err != nil || string(encoded) != "1.234567891"+map[bool]string{true: "\n"}[name == "yaml"] {
				t.Fatalf("encode = %q, %v", encoded, err)
			}
		})
	}
}

func TestUSDRejectsNegativeAndOverprecision(t *testing.T) {
	for _, value := range []string{"-0.000000001", "0.0000000001"} {
		t.Run(value, func(t *testing.T) {
			var jsonValue, yamlValue USD
			if err := json.Unmarshal([]byte(value), &jsonValue); err == nil {
				t.Fatalf("JSON accepted %s", value)
			}
			if err := yaml.Unmarshal([]byte(value), &yamlValue); err == nil {
				t.Fatalf("YAML accepted %s", value)
			}
		})
	}
}

func TestLegacyLimitUsesFortyCentsPerMillionUnits(t *testing.T) {
	if got := LegacyLimit(1_000_000); got != USD(400_000_000) {
		t.Fatalf("LegacyLimit(1M) = %s, want 0.400000000", got)
	}
}
