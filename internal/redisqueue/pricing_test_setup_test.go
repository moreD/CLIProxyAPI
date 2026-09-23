package redisqueue

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestMain(m *testing.M) {
	billing.ReplaceCatalog(map[string]billing.PriceRate{
		"gpt-5.5":     {Input: 5, Output: 30, CacheRead: 0.5},
		"gpt-5.6-sol": {Input: 4, Output: 20, CacheRead: 0.4, ServiceTiers: map[string]billing.PriceRate{"priority": {Input: 8, Output: 40, CacheRead: 0.8}}},
	})
	os.Exit(m.Run())
}
