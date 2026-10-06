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
	if fontPath := config.EnvStr("", "GORGE_IMAGE_MEME_FONT"); fontPath != "" {
		raw, err := os.ReadFile(fontPath)
		if err == nil {
			err = s.SetMemeFont(raw)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid meme font:", err)
			os.Exit(1)
		}
	}
	// 4 KiB UTF-8 captions expand to about 5.5 KiB in base64 headers, beyond
	// Fiber's default 4 KiB request header buffer.
	server := httpx.New(httpx.Config{ListenAddr: cfg.ListenAddr, BodyLimit: "16M", ReadBufferSize: 8 << 10, Ready: s.Ready})
	imagetransform.RegisterRoutes(server.App(), s, cfg.ServiceToken)
	if e = server.Run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
