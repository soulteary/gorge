package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ConduitClient calls Phorge Conduit API methods. It is how the delegate
// handler hands a task back to PHP: worker.execute runs the task class's real
// PhabricatorWorker on the Phorge side and returns its result.
type ConduitClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewConduitClient(baseURL, token string) *ConduitClient {
	return &ConduitClient{
		baseURL: baseURL,
		token:   token,
		// A delegated Phorge worker may legitimately run for the full queue
		// lease (two hours by default). The task context supplied by Consumer
		// carries that lease deadline, so an unrelated fixed client timeout
		// would only create a false transient failure while PHP kept running.
		httpClient: &http.Client{},
	}
}

// ConduitResponse is Conduit's envelope: exactly one of result or the error
// pair is populated, matching Phorge's own API shape.
type ConduitResponse struct {
	Result    json.RawMessage `json:"result"`
	ErrorCode *string         `json:"error_code"`
	ErrorInfo *string         `json:"error_info"`
}

// Call invokes a Conduit method through the gateway.
//
// Phorge's Conduit API does NOT accept a JSON request body: its
// PhabricatorConduitAPIController::decodeConduitParams() rejects
// `Content-Type: application/json` outright ("Use form-encoded data to submit
// parameters to Conduit endpoints"), and the surrounding stack answers a
// non-Conduit request (e.g. a bare GET, or a body it cannot parse) with an
// HTML page rather than a JSON envelope — which is exactly the
// `invalid character '<'` seen when this client used to POST JSON.
//
// So the wire form here mirrors Phorge's own clients (arcanist's
// ConduitClient and the pre-monorepo PhabricatorGoConduitGatewayClient): a
// form-encoded body carrying a single `params` field whose value is the
// JSON-encoded parameter map, with Conduit metadata (including the API token)
// tucked under the `__conduit__` key, and `output=json` to force the JSON
// envelope. The gateway relays this verbatim to upstream `/api/<method>`.
func (c *ConduitClient) Call(ctx context.Context, method string, params map[string]any) (*ConduitResponse, error) {
	if params == nil {
		params = make(map[string]any)
	}

	// Conduit metadata rides under __conduit__. The token authenticates the
	// call to upstream Phorge; the gateway additionally checks its own
	// X-Service-Token header below.
	conduitMeta := map[string]any{}
	if c.token != "" {
		conduitMeta["token"] = c.token
	}
	params["__conduit__"] = conduitMeta

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	form := url.Values{}
	form.Set("params", string(paramsJSON))
	form.Set("output", "json")

	callURL := fmt.Sprintf("%s/api/%s", c.baseURL, method)
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, callURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("X-Service-Token", c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var cr ConduitResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		// A non-JSON body is almost always an HTML error/login page from a
		// misrouted call. Surface a bounded, diagnosable slice of it rather
		// than the raw "invalid character '<'" the decoder would give.
		return nil, fmt.Errorf(
			"conduit %s returned non-JSON (HTTP %d): %s",
			method, resp.StatusCode, snippet(respBody))
	}

	if cr.ErrorCode != nil {
		info := ""
		if cr.ErrorInfo != nil {
			info = *cr.ErrorInfo
		}
		return nil, fmt.Errorf("conduit error [%s]: %s", *cr.ErrorCode, info)
	}
	return &cr, nil
}

// snippet trims a response body to a short, single-line form for error text.
func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
