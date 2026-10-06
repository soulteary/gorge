package filestorage

import (
	"context"
	"errors"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestFetchAddressPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "100.64.1.1", "192.168.1.1", "198.18.0.1", "2001:db8::1", "2002:0808:0808::1", "64:ff9b::808:808", "224.1.1.1"} {
		if publicFetchIP(netip.MustParseAddr(raw), nil) {
			t.Fatalf("allowed %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700::1111"} {
		if !publicFetchIP(netip.MustParseAddr(raw), nil) {
			t.Fatalf("denied %s", raw)
		}
	}
	if publicFetchIP(netip.MustParseAddr("8.8.8.8"), []netip.Prefix{netip.MustParsePrefix("::ffff:0:0/80")}) {
		t.Fatal("mapped IPv6 supernet ignored")
	}
	if publicFetchIP(netip.MustParseAddr("8.8.8.8"), []netip.Prefix{netip.MustParsePrefix("8.8.0.0/16")}) {
		t.Fatal("caller policy ignored")
	}
}
func fixtureFetcher(t *testing.T, handler http.HandlerFunc) *fetcher {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := newFetcher()
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:80" {
			t.Errorf("unchecked dial %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
	}
	return f
}
func TestFetchRedirectAndBounds(t *testing.T) {
	f := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" {
			t.Error("redirect leaked Referer")
		}
		if r.Header.Get("X-Service-Token") != "" {
			t.Error("credential forwarded")
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/body", http.StatusFound)
		case "/private":
			http.Redirect(w, r, "http://127.0.0.1/secret", http.StatusFound)
		case "/encoded":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte("bad"))
		default:
			_, _ = w.Write([]byte("hello"))
		}
	})
	b, e := f.fetch(context.Background(), "http://example.test/redirect?secret=fixture", nil)
	if e != nil || string(b) != "hello" {
		t.Fatalf("%q %v", b, e)
	}
	for _, path := range []string{"private", "encoded"} {
		if _, e = f.fetch(context.Background(), "http://example.test/"+path, nil); e == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	f.limit = 4
	if _, e = f.fetch(context.Background(), "http://example.test/body", nil); !errors.Is(e, errFetchLarge) {
		t.Fatal(e)
	}
}
func TestFetchRejectsBeforeDial(t *testing.T) {
	f := newFetcher()
	f.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unsafe dial")
		return nil, errors.New("dial")
	}
	for _, raw := range []string{"file:///etc/passwd", "http://user:pass@example.com/", "http://example.com:8080/", "http://[::ffff:127.0.0.1]/"} {
		if _, e := f.fetch(context.Background(), raw, nil); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
	}
	if _, e := f.fetch(context.Background(), "https://mixed.test/", nil); e == nil {
		t.Fatal("mixed DNS accepted")
	}
}
func TestFetchUnknownLengthAndCancellation(t *testing.T) {
	f := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("12345"))
	})
	f.limit = 4
	if _, e := f.fetch(context.Background(), "http://example.test/", nil); !errors.Is(e, errFetchLarge) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := f.fetch(ctx, "http://example.test/", nil); e == nil {
		t.Fatal("cancel ignored")
	}
}
func TestFetchRouteRequiresHeaderToken(t *testing.T) {
	app := newTestServer(t)
	r := dispatch(t, app, httptest.NewRequest("POST", "/api/file/fetch?token="+testToken, strings.NewReader(`{"uri":"http://127.0.0.1/"}`)))
	if r.Code != 401 {
		t.Fatalf("query token accepted %d", r.Code)
	}
	req := httptest.NewRequest("POST", "/api/file/fetch", strings.NewReader(`{"uri":"http://127.0.0.1/"}`))
	req.Header.Set("X-Service-Token", testToken)
	req.Header.Set("Content-Type", "application/json")
	r = dispatch(t, app, req)
	if r.Code != 400 {
		t.Fatalf("private response %d %s", r.Code, r.Body)
	}
}

func TestFetchRechecksDNSOnRedirect(t *testing.T) {
	f := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://changed.test/secret", http.StatusFound)
	})
	calls := 0
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		calls++
		if calls == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err := f.fetch(context.Background(), "http://initial.test/", nil); err == nil || calls != 2 {
		t.Fatalf("redirect DNS checks=%d err=%v", calls, err)
	}
}
func TestFetchCapacityAndAdditionalPolicy(t *testing.T) {
	f := newFetcher()
	for i := 0; i < cap(f.slots); i++ {
		f.slots <- struct{}{}
	}
	if _, err := f.fetch(context.Background(), "https://example.test/", nil); !errors.Is(err, errFetchBusy) {
		t.Fatal(err)
	}
	f = fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) { t.Error("blocked request reached server") })
	if _, err := f.fetch(context.Background(), "http://example.test/", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); err == nil {
		t.Fatal("additional policy ignored")
	}
}

func TestFetchDisabledAndMalformedPolicy(t *testing.T) {
	srv := httpx.New(httpx.Config{})
	registerFetchRoutes(srv.App(), "")
	r := dispatch(t, srv.App(), httptest.NewRequest("POST", "/api/file/fetch", strings.NewReader(`{"uri":"https://example.test/"}`)))
	if r.Code != 503 {
		t.Fatalf("anonymous fetch enabled %d", r.Code)
	}
	req := httptest.NewRequest("POST", "/api/file/fetch", strings.NewReader(`{"uri":"https://example.test/","denyCIDRs":["invalid"]}`))
	req.Header.Set("X-Service-Token", testToken)
	req.Header.Set("Content-Type", "application/json")
	r = dispatch(t, newTestServer(t), req)
	if r.Code != 400 {
		t.Fatalf("invalid policy accepted %d", r.Code)
	}
}

func TestFetchHTTPBinaryAndCapabilities(t *testing.T) {
	f := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/partial" {
			w.WriteHeader(206)
		}
		_, _ = w.Write([]byte{'a', 0, 'b'})
	})
	srv := httpx.New(httpx.Config{})
	registerFetch(srv.App(), testToken, f)
	r := do(t, srv.App(), "GET", "/api/file/fetch/meta", nil)
	if r.Code != 200 || !strings.Contains(r.Body, `"protocolVersion":1`) {
		t.Fatalf("capabilities %d %s", r.Code, r.Body)
	}
	for _, tc := range []struct {
		uri    string
		status int
	}{{"http://example.test/body", 200}, {"http://example.test/partial", 502}} {
		req := httptest.NewRequest("POST", "/api/file/fetch", strings.NewReader(`{"uri":"`+tc.uri+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Token", testToken)
		r = dispatch(t, srv.App(), req)
		if r.Code != tc.status {
			t.Fatalf("response %d %s", r.Code, r.Body)
		}
		if tc.status == 200 && (r.Body != "a\x00b" || r.Header.Get("Content-Type") != contentTypeBlob) {
			t.Fatalf("binary response corrupted %+v", r)
		}
	}
	req := httptest.NewRequest("POST", "/api/file/fetch", strings.NewReader(`{"uri":"http://example.test/body","denyCIDRs":["::ffff:8.8.0.0/112"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", testToken)
	r = dispatch(t, srv.App(), req)
	if r.Code != 400 {
		t.Fatalf("mapped blacklist ignored %d", r.Code)
	}
}
