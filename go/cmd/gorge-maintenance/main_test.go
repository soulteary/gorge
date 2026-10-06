package main

import (
	"os"
	"strings"
	"testing"
)

func TestStartupRequiresExplicitDatabaseAndToken(t *testing.T) {
	for _, role := range []string{"CACHE", "CONDUIT", "DAEMON"} {
		t.Setenv("GORGE_MAINTENANCE_"+role+"_DSN", "")
	}
	previous := os.Args
	defer func() { os.Args = previous }()
	os.Args = []string{"maintenance", "serve"}
	if err := run(); err == nil {
		t.Fatal("started without database configuration")
	}
	t.Setenv("GORGE_MAINTENANCE_CACHE_DSN", "user:pass@tcp(127.0.0.1:1)/cache")
	t.Setenv("GORGE_SERVICE_TOKEN", "")
	if err := run(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "token") {
		t.Fatalf("empty status token accepted: %v", err)
	}
	t.Setenv("GORGE_MAINTENANCE_CACHE_DSN", "user:pass@tcp(127.0.0.1:1)/cache?multiStatements=true")
	if err := run(); err == nil {
		t.Fatal("multiStatements enabled")
	}
}
