package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (h *Handler) costUsageAuth(c *gin.Context, index string) *coreauth.Auth {
	if index == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return nil
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return nil
	}
	auth := h.authByIndex(index)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return nil
	}
	return auth
}

// GetAuthCostUsage exposes only usage attributed to the requested credential.
func (h *Handler) GetAuthCostUsage(c *gin.Context) {
	index := strings.TrimSpace(c.Query("auth_index"))
	auth := h.costUsageAuth(c, index)
	if auth == nil {
		return
	}
	now := time.Now()
	// The state reports unavailability if observing the provider window fails.
	_ = coreauth.ObserveAuthCostWindow(auth, now)
	c.JSON(http.StatusOK, authusage.Snapshot(index, now))
}

// PatchAuthCostLimit changes the cap without resetting already accumulated use.
func (h *Handler) PatchAuthCostLimit(c *gin.Context) {
	var req struct {
		AuthIndex string       `json:"auth_index"`
		LimitUSD  *billing.USD `json:"limit_usd"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.LimitUSD == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index and a non-negative limit_usd are required"})
		return
	}
	index := strings.TrimSpace(req.AuthIndex)
	auth := h.costUsageAuth(c, index)
	if auth == nil {
		return
	}
	now := time.Now()
	if err := coreauth.ObserveAuthCostWindow(auth, now); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth cost accounting unavailable"})
		return
	}
	state, err := authusage.SetLimit(index, *req.LimitUSD, now)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to persist auth cost limit"})
		return
	}
	// Rebuild cached availability after increasing or removing an exhausted cap.
	h.authManager.RefreshSchedulerEntry(auth.ID)
	c.JSON(http.StatusOK, state)
}
