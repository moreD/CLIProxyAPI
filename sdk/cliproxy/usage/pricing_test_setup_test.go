package usage

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestMain(m *testing.M) {
	billing.ReplaceCatalog(map[string]billing.PriceRate{
		"gpt-5.5":         {Input: 5, Output: 30, CacheRead: 0.5},
		"gpt-5.6-sol":     {Input: 4, Output: 20, CacheRead: 0.4},
		"gpt-6-astra":     {Input: 10, Output: 50, CacheRead: 1},
		"claude-sonnet-4": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		"gemini-2.5-pro":  {Input: 1.25, Output: 10, CacheRead: 0.125},
	})
	os.Exit(m.Run())
}
