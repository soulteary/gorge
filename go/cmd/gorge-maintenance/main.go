// gorge-maintenance executes only fixed, policy-controlled cleanup templates.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/maintenance/cleanup"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	store := &cleanup.Store{DBs: map[string]*sql.DB{}}
	for _, role := range []string{"cache", "conduit", "daemon"} {
		dsn := os.Getenv("GORGE_MAINTENANCE_" + strings.ToUpper(role) + "_DSN")
		if dsn == "" {
			continue
		}
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return fmt.Errorf("invalid %s DSN", role)
		}
		if cfg.DBName == "" || cfg.MultiStatements {
			return fmt.Errorf("%s requires target database and disabled multiStatements", role)
		}
		cfg.Timeout = 3 * time.Second
		cfg.ReadTimeout = 3 * time.Second
		cfg.WriteTimeout = 3 * time.Second
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			return fmt.Errorf("invalid %s database", role)
		}
		db.SetMaxOpenConns(3)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)
		defer db.Close()
		store.DBs[role] = db
	}
	if len(store.DBs) == 0 {
		return fmt.Errorf("configure GORGE_MAINTENANCE_<CACHE|CONDUIT|DAEMON>_DSN")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	enc := json.NewEncoder(os.Stdout)
	if args[0] != "serve" {
		bounded, done := context.WithTimeout(ctx, 40*time.Second)
		defer done()
		switch args[0] {
		case "list":
			if len(args) != 1 {
				return fmt.Errorf("list takes no arguments")
			}
			states := []cleanup.State{}
			for _, spec := range cleanup.Specs() {
				if store.DBs[spec.Role] == nil {
					continue
				}
				st, err := store.Status(bounded, spec.ID)
				if err != nil {
					return err
				}
				states = append(states, st)
			}
			return enc.Encode(states)
		case "import":
			if len(args) != 2 {
				return fmt.Errorf("import <PHP-export.json>")
			}
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			e, err := cleanup.Decode(f)
			if err != nil {
				return err
			}
			for _, p := range e.Policies {
				if err = store.ValidateSchema(bounded, p.ID); err != nil {
					return err
				}
			}
			for _, p := range e.Policies {
				if err = store.Import(bounded, p); err != nil {
					return fmt.Errorf("%s import failed (earlier collectors may already be imported): %w", p.ID, err)
				}
				if err = enc.Encode(map[string]string{"imported": p.ID}); err != nil {
					return err
				}
			}
			return nil
		case "pause", "resume", "dry-run", "run-once":
			if args[0] == "resume" {
				if len(args) != 3 {
					return fmt.Errorf("resume <collector> <php|gorge>")
				}
				if args[2] != "php" && args[2] != "gorge" {
					return fmt.Errorf("resume owner must be php or gorge")
				}
				if err := store.ValidateSchema(bounded, args[1]); err != nil {
					return err
				}
				return store.SetOwner(bounded, args[1], args[2])
			}
			if len(args) != 2 {
				return fmt.Errorf("%s <collector>", args[0])
			}
			switch args[0] {
			case "pause":
				return store.SetOwner(bounded, args[1], "paused")
			case "dry-run":
				if err := store.ValidateSchema(bounded, args[1]); err != nil {
					return err
				}
				ids, err := store.DryRun(bounded, args[1])
				if err != nil {
					return err
				}
				return enc.Encode(map[string]any{"collector": args[1], "sampleIDs": ids, "boundedSample": true})
			case "run-once":
				n, err := store.RunOnce(bounded, args[1])
				if err != nil {
					return err
				}
				return enc.Encode(map[string]any{"collector": args[1], "deleted": n})
			}
		}
		return fmt.Errorf("commands: serve, list, import, dry-run, run-once, pause, resume")
	}
	if len(args) != 1 {
		return fmt.Errorf("serve takes no arguments")
	}
	token := os.Getenv("GORGE_SERVICE_TOKEN")
	if token == "" {
		return fmt.Errorf("nonempty GORGE_SERVICE_TOKEN required")
	}
	addr := os.Getenv("GORGE_LISTEN_ADDR")
	if addr == "" {
		addr = ":8200"
	}
	srv := httpx.New(httpx.Config{ListenAddr: addr, Ready: func() error {
		probe, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		return store.Ready(probe)
	}})
	cleanup.RegisterRoutes(srv.App(), store, token)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); store.Run(ctx) }()
	err := srv.Run()
	cancel()
	wg.Wait()
	return err
}
