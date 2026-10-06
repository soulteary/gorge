package main

import (
	"fmt"
	"github.com/soulteary/gorge/go/internal/imagetransform"
	"github.com/soulteary/gorge/go/internal/platform/config"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"os"
	"time"
)

func main() {
	cfg := config.LoadBase(":8190")
	if cfg.ServiceToken == "" {
		fmt.Fprintln(os.Stderr, "gorge-image requires GORGE_SERVICE_TOKEN")
		os.Exit(1)
	}
	s, e := imagetransform.New(config.EnvStr("convert", "GORGE_IMAGE_BINARY"), config.EnvStr("/etc/gorge/image", "GORGE_IMAGE_POLICY_DIR"), config.EnvInt(2, "GORGE_IMAGE_CONCURRENCY"), time.Duration(config.EnvInt(10, "GORGE_IMAGE_TIMEOUT_SEC"))*time.Second)
	if e != nil {
		fmt.Fprintln(os.Stderr, "gorge-image initialization failed:", e)
		os.Exit(1)
	}
	server := httpx.New(httpx.Config{ListenAddr: cfg.ListenAddr, BodyLimit: "16M", Ready: s.Ready})
	imagetransform.RegisterRoutes(server.App(), s, cfg.ServiceToken)
	if e = server.Run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
