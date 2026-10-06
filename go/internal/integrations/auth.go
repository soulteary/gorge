package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Authentication tokens are returned only to the requesting PHP session;
// neither requests nor responses enter the durable side-effect ledger.
type AuthRequest struct {
	Target    string            `json:"target"`
	Operation string            `json:"operation"`
	Secret    string            `json:"secret"`
	Params    map[string]string `json:"params"`
}

func (r AuthRequest) Validate(t Target) error {
	allowed := map[string]bool{}
	switch t.Type {
	case "github":
		if r.Operation != "token" || r.Params["client_id"] == "" || r.Params["client_secret"] == "" || r.Params["code"] == "" {
			return errors.New("invalid GitHub token request")
		}
		for _, k := range []string{"client_id", "client_secret", "redirect_uri", "code"} {
			allowed[k] = true
		}
	case "asana":
		if r.Operation != "token" || r.Params["client_id"] == "" || r.Params["client_secret"] == "" {
			return errors.New("invalid Asana token request")
		}
		switch r.Params["grant_type"] {
		case "authorization_code":
			if r.Params["code"] == "" {
				return errors.New("missing code")
			}
		case "refresh_token":
			if r.Params["refresh_token"] == "" {
				return errors.New("missing refresh token")
			}
		default:
			return errors.New("invalid grant")
		}
		for _, k := range []string{"client_id", "client_secret", "redirect_uri", "grant_type", "code", "refresh_token"} {
			allowed[k] = true
		}
	case "jira":
		switch r.Operation {
		case "request-token":
			if r.Secret != "" {
				return errors.New("unexpected token")
			}
			allowed["oauth_callback"] = true
		case "access-token":
			if r.Secret == "" || r.Params["oauth_verifier"] == "" {
				return errors.New("missing verifier")
			}
			allowed["oauth_verifier"] = true
		default:
			return errors.New("invalid OAuth1 operation")
		}
	default:
		return errors.New("unsupported authentication target")
	}
	if len(r.Secret) > 8192 || strings.ContainsAny(r.Secret, "\r\n") {
		return errors.New("oversized token")
	}
	for k, v := range r.Params {
		if !allowed[k] || len(v) > 8192 {
			return errors.New("invalid token parameter")
		}
	}
	return nil
}
func Authenticate(ctx context.Context, client *http.Client, t Target, r AuthRequest) (Outcome, error) {
	if e := r.Validate(t); e != nil {
		return Outcome{}, e
	}
	values := url.Values{}
	for k, v := range r.Params {
		values.Set(k, v)
	}
	uri := strings.TrimRight(t.URL, "/") + "/plugins/servlet/oauth/" + r.Operation
	if t.Type == "github" {
		uri = "https://github.com/login/oauth/access_token"
	}
	if t.Type == "asana" {
		base, e := url.Parse(t.URL)
		if e != nil {
			return Outcome{}, e
		}
		base.Path = "/-/oauth_token"
		uri = base.String()
	}
	bodyValues := url.Values{}
	for k, vs := range values {
		if t.Type == "jira" && k == "oauth_callback" {
			continue
		}
		bodyValues[k] = vs
	}
	req, e := http.NewRequestWithContext(ctx, "POST", uri, strings.NewReader(bodyValues.Encode()))
	if e != nil {
		return Outcome{}, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if t.Type == "jira" {
		header, e := oauthHeader(t, r.Secret, "POST", uri, values)
		if e != nil {
			return Outcome{}, e
		}
		req.Header.Set("Authorization", header)
	}
	resp, e := client.Do(req)
	if e != nil {
		return Outcome{}, errors.New("authentication outcome unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(body) > 65536 {
		return Outcome{}, errors.New("authentication response unavailable")
	}
	out := Outcome{State: "auth", Status: resp.StatusCode, Result: json.RawMessage(`{}`)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, nil
	}
	if t.Type != "jira" {
		var data map[string]any
		if json.Unmarshal(body, &data) != nil {
			return Outcome{}, errors.New("invalid token response")
		}
		token, ok := data["access_token"].(string)
		if !ok || token == "" {
			return Outcome{}, errors.New("invalid token response")
		}
		out.Result = body
	} else {
		data, e := url.ParseQuery(string(body))
		if e != nil || data.Get("oauth_token") == "" || data.Get("oauth_token_secret") == "" {
			return Outcome{}, errors.New("invalid OAuth1 response")
		}
		result := map[string]string{}
		for _, k := range []string{"oauth_token", "oauth_token_secret", "oauth_callback_confirmed"} {
			if v := data.Get(k); v != "" {
				result[k] = v
			}
		}
		out.Result = mustJSON(result)
	}
	return out, nil
}
