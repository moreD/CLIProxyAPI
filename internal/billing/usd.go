// Package billing owns client dollar accounting, independent of the management UI.
package billing

import (
	"fmt"
	"math/big"
	"strings"

	"gopkg.in/yaml.v3"
)

// USD stores nanodollars as integers; JSON and YAML always expose dollars.
type USD int64

const Scale int64 = 1_000_000_000

func (v USD) String() string {
	return new(big.Rat).SetFrac(big.NewInt(int64(v)), big.NewInt(Scale)).FloatString(9)
}

func (v USD) MarshalJSON() ([]byte, error) { return []byte(v.String()), nil }
func (v USD) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: v.String()}, nil
}
func (v *USD) UnmarshalJSON(data []byte) error     { return v.parse(string(data)) }
func (v *USD) UnmarshalYAML(node *yaml.Node) error { return v.parse(node.Value) }
func (v *USD) parse(text string) error {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok || r.Sign() < 0 {
		return fmt.Errorf("USD amount must be a non-negative number")
	}
	r.Mul(r, new(big.Rat).SetInt64(Scale))
	if !r.IsInt() || !r.Num().IsInt64() {
		return fmt.Errorf("USD amount is out of range or has more than 9 decimal places")
	}
	*v = USD(r.Num().Int64())
	return nil
}

// LegacyLimit converts old quota units using GPT-5.6 Sol input/cache rates:
// 1M old units = $0.40. This is only used to import legacy configuration.
func LegacyLimit(units int64) USD {
	if units <= 0 {
		return 0
	}
	if units > (1<<63-1)/400 {
		return USD(1<<63 - 1)
	}
	return USD(units * 400)
}

func Add(a, b USD) USD {
	if b > 0 && a > USD(1<<63-1)-b {
		return USD(1<<63 - 1)
	}
	return a + b
}
