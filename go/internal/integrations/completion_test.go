package integrations

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/gofiber/fiber/v3"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRawMIMEMigration(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: task@example.com\r\nMessage-ID: <raw-one>\r\nSubject: =?UTF-8?B?5rWL6K+V?=\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n--inner\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<b>hello</b>\r\n--inner--\r\n--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=UTF-8''report%20one.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJj\r\n--outer--\r\n"
	in := Intake{Provider: "raw", Raw: base64.StdEncoding.EncodeToString([]byte(raw)), IngressID: strings.Repeat("a", 32)}
	m, e := Normalize(in)
	if e != nil || m.Text != "café" || m.HTML != "<b>hello</b>" || m.Headers["subject"] != "测试" || len(m.Attachments) != 1 || m.Attachments[0].Name != "report one.txt" || m.Attachments[0].Data != "YWJj" {
		t.Fatalf("MIME semantics: %#v %v", m, e)
	}
	in.ProcessDuplicates = true
	m, e = Normalize(in)
	if e != nil || m.Headers["message-id"] != in.IngressID {
		t.Fatal("explicit debug duplicates lost", e)
	}
	for _, raw := range []string{"not a mail", "From: a\nTo: b\nContent-Type: multipart/mixed\n\nmissing", "From: a\nTo: b\nContent-Transfer-Encoding: base64\n\n!corrupt!"} {
		if _, e := Normalize(Intake{Provider: "raw", Raw: base64.StdEncoding.EncodeToString([]byte(raw)), IngressID: strings.Repeat("a", 32)}); e == nil {
			t.Fatal("malformed mail acknowledged")
		}
	}
}
func TestGitHubPollingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues/events" || r.URL.Query().Get("page") != "2" || r.Header.Get("If-None-Match") != `"previous"` {
			t.Error("pagination or conditional request lost")
		}
		w.Header().Set("ETag", `"next"`)
		w.Header().Set("X-Poll-Interval", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "999")
		w.Header().Set("Set-Cookie", "secret")
		w.WriteHeader(304)
	}))
	defer server.Close()
	r := Request{ID: "read", Path: "repos/owner/repo/issues/events", Method: "GET", Secret: "oauth", Principal: "source", ETag: `"previous"`, Params: map[string]any{"page": float64(2)}}
	if e := r.Validate(Target{Type: "github"}); e != nil {
		t.Fatal(e)
	}
	out, e := SendRead(context.Background(), server.Client(), Target{Type: "github", URL: server.URL}, r)
	if e != nil || out.Status != 304 || out.Headers["etag"] != `"next"` || out.Headers["x-ratelimit-remaining"] != "0" || out.Headers["set-cookie"] != "" {
		t.Fatalf("poll metadata %#v %v", out, e)
	}
	r.ETag = "bad\r\nInjected: value"
	if r.Validate(Target{Type: "github"}) == nil {
		t.Fatal("unsafe ETag accepted")
	}
}
func TestAuthenticationExchanges(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	signing := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseForm(); e != nil {
			t.Error(e)
		}
		switch r.URL.Path {
		case "/-/oauth_token":
			if r.Form.Get("client_secret") != "client-secret" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh" {
				t.Error("OAuth2 exchange changed")
			}
			_, _ = w.Write([]byte(`{"access_token":"new-token","expires_in":3600}`))
		case "/plugins/servlet/oauth/request-token":
			if !strings.Contains(r.Header.Get("Authorization"), "oauth_callback=") || strings.Contains(r.Header.Get("Authorization"), "oauth_token=") {
				t.Error("OAuth1 callback changed")
			}
			_, _ = w.Write([]byte("oauth_token=request&oauth_token_secret=secret&oauth_callback_confirmed=true"))
		case "/plugins/servlet/oauth/access-token":
			if r.PostForm.Get("oauth_verifier") != "verify" || !strings.Contains(r.Header.Get("Authorization"), `oauth_token="request"`) {
				t.Error("OAuth1 verifier changed")
			}
			_, _ = w.Write([]byte("oauth_token=access&oauth_token_secret=secret"))
		default:
			t.Error("authentication escaped fixed endpoint")
		}
	}))
	defer server.Close()
	for _, r := range []AuthRequest{
		{Operation: "token", Params: map[string]string{"client_id": "client", "client_secret": "client-secret", "grant_type": "refresh_token", "refresh_token": "refresh"}},
		{Operation: "request-token", Params: map[string]string{"oauth_callback": "https://phorge.example/auth/"}},
		{Operation: "access-token", Secret: "request", Params: map[string]string{"oauth_verifier": "verify"}},
	} {
		kind := "jira"
		if r.Operation == "token" {
			kind = "asana"
		}
		out, e := Authenticate(context.Background(), server.Client(), Target{Type: kind, URL: server.URL, ConsumerKey: "consumer", PrivateKey: signing}, r)
		if e != nil || out.Status != 200 || !json.Valid(out.Result) {
			t.Fatalf("auth exchange %#v %v", out, e)
		}
	}
	if (AuthRequest{Operation: "token", Params: map[string]string{"client_id": "id", "client_secret": "secret", "grant_type": "client_credentials"}}).Validate(Target{Type: "asana"}) == nil {
		t.Fatal("unsupported grant accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGitHubAuthenticationFixedHost(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://github.com/login/oauth/access_token" || r.Header.Get("Accept") != "application/json" {
			t.Error("GitHub token endpoint changed")
		}
		if e := r.ParseForm(); e != nil {
			t.Error(e)
		}
		if r.PostForm.Get("code") != "one-time-code" {
			t.Error("code lost")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"github-token","scope":"repo"}`))}, nil
	})}
	out, e := Authenticate(context.Background(), client, Target{Type: "github", URL: "https://api.github.com"}, AuthRequest{Operation: "token", Params: map[string]string{"client_id": "id", "client_secret": "secret", "code": "one-time-code"}})
	if e != nil || out.Status != 200 || calls != 1 {
		t.Fatal("GitHub exchange", out, e, calls)
	}
}

func TestDurableResultDoesNotRetainProviderContent(t *testing.T) {
	for kind, raw := range map[string]string{
		"twilio": `{"sid":"id","body":"private SMS","auth_token":"secret"}`,
		"asana":  `{"gid":"id","notes":"private notes","name":"private title"}`,
		"jira":   `{"id":"id","body":"private comment"}`,
		"sns":    `{"messageID":"id","body":"private message"}`,
	} {
		result := string(minimalResult(kind, json.RawMessage(raw)))
		if strings.Contains(result, "private") || strings.Contains(result, "secret") || !strings.Contains(result, "id") {
			t.Fatalf("%s retains provider content: %s", kind, result)
		}
	}
}

func TestFactProgressReportsFailuresAndRecovery(t *testing.T) {
	s := New(Config{FactDSN: "enabled"}, Store{})
	s.RecordFactResult(errors.New("source unavailable"))
	if h := s.FactHealth(); !h.Enabled || h.ConsecutiveFailures != 1 || h.LastSuccess != 0 {
		t.Fatal("projection failure hidden", h)
	}
	s.RecordFactResult(nil)
	if h := s.FactHealth(); h.ConsecutiveFailures != 0 || h.LastSuccess == 0 {
		t.Fatal("projection recovery hidden", h)
	}
	s.progress.mu.Lock()
	s.progress.started = 1
	s.progress.fact.LastSuccess = 1
	s.progress.mu.Unlock()
	if !s.FactHealth().Stale {
		t.Fatal("stalled projection appears current")
	}
}

func TestHTTPAuthenticationAndReadContracts(t *testing.T) {
	service := New(Config{Token: "contract", Targets: map[string]Target{"asana": {Type: "asana", URL: "https://app.asana.com/api/1.0"}}}, Store{})
	calls := 0
	service.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"data":{"gid":"user"}}`
		if r.URL.Path == "/-/oauth_token" {
			body = `{"access_token":"private-token"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	app := fiber.New()
	service.Register(app)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/auth", `{"target":"asana","operation":"token","params":{"client_id":"client","client_secret":"secret","code":"once","grant_type":"authorization_code"}}`, 200},
		{"/read", `{"target":"asana","principal":"actor","secret":"token","method":"GET","path":"users/me","params":{}}`, 200},
		{"/read", `{"target":"asana","principal":"actor","secret":"token","method":"GET","path":"users/me","params":[]}`, 400},
		{"/effect", `{"target":"asana","id":"identity","principal":"actor","secret":"token","method":"GET","path":"users/me"}`, 400},
	} {
		req := httptest.NewRequest("POST", "/api/integrations"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Token", "contract")
		resp, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: HTTP %d", tc.path, resp.StatusCode)
		}
		if tc.path == "/auth" && resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("authentication token can be cached")
		}
	}
	req := httptest.NewRequest("POST", "/api/integrations/auth?token=contract", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, e := app.Test(req)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 || calls != 2 {
		t.Fatal("unvalidated requests reached the provider", resp.StatusCode, calls)
	}
}
