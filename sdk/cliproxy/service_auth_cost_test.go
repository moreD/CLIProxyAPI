package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsUnavailableAuthAccountingBeforeServerStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("usage-stats", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("usage-stats", "auth-usage"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	// No providers or server are initialized: the accounting error must return
	// before Run can touch any listener or later lifecycle dependency.
	s := &Service{}
	if err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "initialize auth dollar accounting") {
		t.Fatalf("Run error = %v, want accounting initialization failure", err)
	}
}
