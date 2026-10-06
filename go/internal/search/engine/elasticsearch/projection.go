package elasticsearch

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

// ApplyProjection uses strict external versions. Deletes remain full replacement
// tombstones so version protection survives Elasticsearch's delete GC window.
// A 409 is not success until a realtime GET confirms compatible stored metadata.
func (b *Backend) ApplyProjection(ctx context.Context, e *contracts.SearchProjection, target projection.Target) (string, error) {
	revision, err := projection.Validate(e)
	if err != nil {
		return "", err
	}
	if !b.projectionMode || b.version < 7 || b.version > 8 {
		return "", fmt.Errorf("projection requires Elasticsearch 7 or 8 and projection mode")
	}
	host, err := b.hostForRole("write")
	if err != nil {
		return "", err
	}
	body := map[string]any{"docType": e.Type}
	if e.Document != nil {
		body = b.buildDocSpec(e.Document)
	}
	body["_gorge"] = map[string]any{"namespace": e.Namespace, "generation": target.GenerationID, "hash": e.PayloadHash, "deleted": e.Operation == "delete"}
	uri := b.baseURL(host) + "/_doc/" + e.PHID
	code, raw, err := b.projectionRequest(ctx, http.MethodPut, uri+"?version_type=external&version="+e.Revision, body)
	if err != nil {
		return "", err
	}
	if code == 200 || code == 201 {
		var ack struct {
			Version int64  `json:"_version"`
			ID      string `json:"_id"`
			Result  string `json:"result"`
			Shards  *struct {
				Failed int `json:"failed"`
			} `json:"_shards"`
		}
		if json.Unmarshal(raw, &ack) != nil || ack.Version != revision || ack.ID != e.PHID || (ack.Result != "created" && ack.Result != "updated") || ack.Shards == nil || ack.Shards.Failed != 0 {
			return "", fmt.Errorf("invalid projection write receipt")
		}
		return "applied", nil
	}
	if code != 409 {
		return "", fmt.Errorf("projection backend HTTP %d", code)
	}
	code, raw, err = b.projectionRequest(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("projection conflict could not be verified")
	}
	var current struct {
		Version int64  `json:"_version"`
		Found   bool   `json:"found"`
		ID      string `json:"_id"`
		Source  struct {
			Meta struct {
				Namespace  string `json:"namespace"`
				Generation string `json:"generation"`
				Hash       string `json:"hash"`
				Deleted    *bool  `json:"deleted"`
			} `json:"_gorge"`
		} `json:"_source"`
	}
	if json.Unmarshal(raw, &current) != nil || !current.Found || current.ID != e.PHID || current.Source.Meta.Namespace != e.Namespace || current.Source.Meta.Generation != target.GenerationID || current.Source.Meta.Deleted == nil || len(current.Source.Meta.Hash) != 64 {
		return "", fmt.Errorf("incompatible projection conflict")
	}
	if _, err := hex.DecodeString(current.Source.Meta.Hash); err != nil {
		return "", fmt.Errorf("invalid projection conflict hash")
	}
	if current.Version > revision {
		return "superseded", nil
	}
	if current.Version == revision && current.Source.Meta.Hash == e.PayloadHash && *current.Source.Meta.Deleted == (e.Operation == "delete") {
		return "applied", nil
	}
	return "", fmt.Errorf("projection revision conflict")
}
func (b *Backend) projectionRequest(ctx context.Context, method, uri string, body any) (int, []byte, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, uri, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, fmt.Errorf("invalid projection request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("projection backend request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil || len(raw) > 4*1024*1024 {
		return 0, nil, fmt.Errorf("invalid projection backend response")
	}
	return resp.StatusCode, raw, nil
}

// ProjectionIndexUUID refuses missing indices and aliases. The persisted UUID
// prevents a recreated physical generation from silently losing revision fences.
func (b *Backend) ProjectionIndexUUID(ctx context.Context) (string, error) {
	host, err := b.hostForRole("write")
	if err != nil {
		return "", err
	}
	code, raw, err := b.projectionRequest(ctx, http.MethodGet, b.baseURL(host)+"/_settings", nil)
	if err != nil {
		return "", err
	}
	var settings map[string]struct {
		Settings struct {
			Index struct {
				UUID string `json:"uuid"`
			} `json:"index"`
		} `json:"settings"`
	}
	if code != 200 || json.Unmarshal(raw, &settings) != nil || len(settings) != 1 || settings[b.index].Settings.Index.UUID == "" {
		return "", fmt.Errorf("projection requires an existing physical index")
	}
	// The root response distinguishes actual server versions from config guesses.
	base := b.baseURL(host)
	base = base[:len(base)-len(b.index)]
	code, raw, err = b.projectionRequest(ctx, http.MethodGet, base, nil)
	if err != nil {
		return "", err
	}
	var server struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if code != 200 || json.Unmarshal(raw, &server) != nil || len(server.Version.Number) < 2 || server.Version.Number[:2] != strconv.Itoa(b.version)+"." {
		return "", fmt.Errorf("projection Elasticsearch version mismatch")
	}
	code, raw, err = b.projectionRequest(ctx, http.MethodGet, b.baseURL(host)+"/_mapping", nil)
	if err != nil {
		return "", err
	}
	var mapping map[string]struct {
		Mappings struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if code != 200 || json.Unmarshal(raw, &mapping) != nil {
		return "", fmt.Errorf("projection mapping unavailable")
	}
	properties := mapping[b.index].Mappings.Properties["_gorge"].Properties
	for field, typ := range map[string]string{"namespace": "keyword", "generation": "keyword", "hash": "keyword", "deleted": "boolean"} {
		if properties[field].Type != typ {
			return "", fmt.Errorf("projection metadata mapping required")
		}
	}
	return settings[b.index].Settings.Index.UUID, nil
}
