package main

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

func TestBuildDepsUsesPasswordResolvedByClusterFile(t *testing.T) {
	cfg := &dbapi.Config{MySQLPass: "environment-password"}
	cfg.ServiceToken = "token"
	cluster := &dbapi.ClusterConfig{MySQLPass: "file-password"}

	deps := buildDeps(cfg, cluster)
	if deps.Password != "file-password" {
		t.Fatalf("password = %q, want the value resolved from the cluster file", deps.Password)
	}
}
