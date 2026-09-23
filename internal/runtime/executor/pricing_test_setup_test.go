package executor

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestMain(m *testing.M) {
	billing.ReplaceCatalog(map[string]billing.PriceRate{
		"gpt-5.6-terra": {Input: 2, Output: 12, CacheRead: 0.2},
		"gpt-6-astra":   {Input: 10, Output: 50, CacheRead: 1},
	})
	os.Exit(m.Run())
}
