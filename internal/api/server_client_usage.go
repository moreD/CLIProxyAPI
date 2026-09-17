package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

func (s *Server) serveClientUsagePage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	staticDir := managementasset.StaticDir(s.configFilePath)
	if staticDir == "" {
		c.Status(http.StatusNotFound)
		return
	}
	filePath := filepath.Join(staticDir, "usage.html")
	if info, err := os.Stat(filePath); err != nil || !info.Mode().IsRegular() {
		c.Status(http.StatusNotFound)
		return
	}
	c.File(filePath)
}

func (s *Server) getClientUsage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || s.accessManager == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
		return
	}
	result, errAuth := s.accessManager.Authenticate(c.Request.Context(), c.Request)
	if errAuth != nil && errAuth.HTTPStatusCode() >= http.StatusInternalServerError {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication unavailable"})
		return
	}
	if errAuth != nil || result == nil || result.Principal != parts[1] {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
		return
	}
	// Usage lookup must remain available after the client's inference budget is exhausted.
	snapshot, available := redisqueue.ClientUsageSnapshotNow(result.Principal)
	if !available {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Usage statistics unavailable"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}
