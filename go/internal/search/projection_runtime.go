package search

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/engine/elasticsearch"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type ProjectionBackend struct {
	projection.Target
	Backend engine.BackendDef `json:"backend"`
}

var physicalIndexPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)

// PrepareProjectionDelivery only enables separate shadow physical generations.
// Rebuild validation and read cutover are separate, not implied by activation.
func PrepareProjectionDelivery(ctx context.Context, cfg *ProjectionConfig, legacy []engine.BackendDef, db *sql.DB) ([]*projection.Worker, error) {
	if cfg == nil || len(cfg.Deliveries) == 0 {
		return nil, nil
	}
	if err := projection.ValidateNamespace(cfg.Namespace); err != nil {
		return nil, err
	}
	if err := projection.ValidateTargets(cfg.Targets); err != nil {
		return nil, err
	}
	if len(cfg.Deliveries) != len(cfg.Targets) {
		return nil, fmt.Errorf("all projection targets require delivery configuration")
	}
	wanted := map[projection.Target]bool{}
	for _, t := range cfg.Targets {
		wanted[t] = true
	}
	for _, d := range cfg.Deliveries {
		if !wanted[d.Target] {
			return nil, fmt.Errorf("unknown or duplicate projection delivery target")
		}
		delete(wanted, d.Target)
		b := d.Backend
		if b.Type != "elasticsearch" || (b.Version != 7 && b.Version != 8) || len(b.Hosts) != 1 || !physicalIndexPattern.MatchString(b.Index) || b.APIKey != "" || b.Timeout < 0 || b.Timeout > 60 {
			return nil, fmt.Errorf("delivery requires one Elasticsearch 7/8 endpoint and explicit physical index")
		}
		if b.Protocol != "" && b.Protocol != "http" && b.Protocol != "https" {
			return nil, fmt.Errorf("invalid projection protocol")
		}
		endpoint := b.Hosts[0]
		if b.Protocol == "" {
			b.Protocol = "http"
		}
		if parsed, err := url.Parse(endpoint); err == nil && parsed.User != nil {
			return nil, fmt.Errorf("projection URL credentials are unsupported")
		}
		if b.Options["projection"] != "true" {
			return nil, fmt.Errorf("projection backend mode is required")
		}
		for _, live := range legacy {
			index := live.Index
			if index == "" {
				index = engine.DefaultIndexName
			}
			if index == b.Index {
				return nil, fmt.Errorf("projection shadow index must differ from legacy indices")
			}
		}
	}
	if db == nil {
		return nil, fmt.Errorf("projection control database required")
	}
	var workers []*projection.Worker
	for _, d := range cfg.Deliveries {
		backend := elasticsearch.New(d.Backend)
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		uuid, err := backend.ProjectionIndexUUID(probeCtx)
		cancel()
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(struct {
			Backend engine.BackendDef
			UUID    string
		}{d.Backend, uuid})
		sum := sha256.Sum256(raw)
		bindCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = (&projection.MySQLStore{DB: db}).BindTarget(bindCtx, cfg.Namespace, d.Target, hex.EncodeToString(sum[:]), uuid)
		cancel()
		if err != nil {
			return nil, err
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		workers = append(workers, &projection.Worker{Store: &projection.MySQLStore{DB: db}, Writer: backend, Namespace: cfg.Namespace, Target: d.Target, Owner: "search-" + hex.EncodeToString(nonce[:])})
	}
	return workers, nil
}
