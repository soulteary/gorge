package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type SourceScanConfig struct {
	ConduitURL string `json:"conduitURL"`
}
type conduitSourceProvider struct{ client *conduitclient.Client }

func NewSourceProvider(cfg *SourceScanConfig, token string) (projection.SourceProvider, error) {
	if cfg == nil || token == "" {
		return nil, fmt.Errorf("source scan requires configured Conduit URL and source token")
	}
	u, err := url.Parse(cfg.ConduitURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid source Conduit URL")
	}
	return &conduitSourceProvider{client: conduitclient.NewBounded(strings.TrimRight(cfg.ConduitURL, "/"), token, 4*1024*1024)}, nil
}
func (p *conduitSourceProvider) call(ctx context.Context, params map[string]any, result any) error {
	r, err := p.client.Call(ctx, "search.source", params)
	if err != nil {
		return fmt.Errorf("source service unavailable")
	}
	dec := json.NewDecoder(bytes.NewReader(r.Result))
	dec.DisallowUnknownFields()
	if err = dec.Decode(result); err != nil {
		return fmt.Errorf("invalid source response")
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid trailing source response")
	}
	return nil
}
func (p *conduitSourceProvider) Catalog(ctx context.Context) (*projection.SourceCatalog, error) {
	var catalog projection.SourceCatalog
	err := p.call(ctx, map[string]any{"phase": "catalog"}, &catalog)
	return &catalog, err
}
func (p *conduitSourceProvider) Scan(ctx context.Context, class, after, upper string) (*projection.SourcePage, error) {
	var page projection.SourcePage
	err := p.call(ctx, map[string]any{"phase": "scan", "className": class, "afterID": after, "upperID": upper}, &page)
	return &page, err
}
