package filestorage

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const fetchLimit = 16 << 20

var errFetchDenied = errors.New("destination is not permitted")
var errFetchLarge = errors.New("download exceeds size limit")
var errFetchBusy = errors.New("download capacity unavailable")
var fetchDeniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

type fetcher struct {
	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	slots  chan struct{}
	limit  int64
}

func newFetcher() *fetcher {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &fetcher{lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}, dial: d.DialContext, slots: make(chan struct{}, 4), limit: fetchLimit}
}
func publicFetchIP(ip netip.Addr, deny []netip.Prefix) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range fetchDeniedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	for _, p := range deny {
		if p.Contains(ip) || (ip.Is4() && p.Contains(netip.AddrFrom16(ip.As16()))) {
			return false
		}
	}
	return true
}
func validFetchURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Opaque == "" &&
		(u.Port() == "" || (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443"))
}

// Resolve at connection time, reject the entire mixed answer, then dial a
// checked literal IP. The URL hostname remains intact for Host and TLS SNI.
func (f *fetcher) fetch(ctx context.Context, raw string, deny []netip.Prefix) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || !validFetchURL(u) {
		return nil, errFetchDenied
	}
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		return nil, errFetchBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errFetchDenied
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = f.lookup(ctx, host)
			if err != nil {
				return nil, errFetchDenied
			}
		}
		if len(ips) == 0 {
			return nil, errFetchDenied
		}
		for _, ip := range ips {
			if !publicFetchIP(ip, deny) {
				return nil, errFetchDenied
			}
		}
		for _, ip := range ips {
			conn, err := f.dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("download connection failed")
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		if len(via) > 10 || !validFetchURL(req.URL) {
			return errFetchDenied
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errFetchDenied
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errFetchDenied) {
			return nil, errFetchDenied
		}
		return nil, errors.New("download request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("download returned unsuccessful status")
	}
	if resp.ContentLength > f.limit {
		return nil, errFetchLarge
	}
	if v := resp.Header.Get("Content-Encoding"); v != "" && !strings.EqualFold(v, "identity") {
		return nil, errors.New("encoded download is unsupported")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.limit+1))
	if err != nil {
		return nil, errors.New("download body failed")
	}
	if int64(len(data)) > f.limit {
		return nil, errFetchLarge
	}
	return data, nil
}
func registerFetchRoutes(app fiber.Router, token string) {
	registerFetch(app, token, newFetcher())
}

func registerFetch(app fiber.Router, token string, f *fetcher) {
	g := app.Group("/api/file/fetch")
	// Fetch never runs anonymously, even when development blob routes do.
	g.Use(func(c fiber.Ctx) error {
		if token == "" {
			return httpx.Fail(c, 503, "ERR_FETCH_DISABLED", "download requires a service token")
		}
		return c.Next()
	})
	g.Use(auth.Token(token, auth.WithQueryToken(false)))
	g.Get("/meta", func(c fiber.Ctx) error {
		return httpx.OK(c, map[string]any{"protocolVersion": 1, "maxBytes": fetchLimit,
			"publicOnly": true, "headerTokenOnly": true, "pinnedDNS": true})
	})
	g.Post("", func(c fiber.Ctx) error {
		var req struct {
			URI  string   `json:"uri"`
			Deny []string `json:"denyCIDRs"`
		}
		if err := c.Bind().Body(&req); err != nil || len(req.Deny) > 256 {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid download request")
		}
		var deny []netip.Prefix
		for _, raw := range req.Deny {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				ip, e := netip.ParseAddr(raw)
				if e != nil {
					return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid deny address")
				}
				p = netip.PrefixFrom(ip, ip.BitLen())
			}
			if p.Addr().Is4In6() && p.Bits() >= 96 {
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			deny = append(deny, p)
		}
		data, err := f.fetch(c.Context(), req.URI, deny)
		if err != nil {
			status := 502
			code := "ERR_FETCH_FAILED"
			if errors.Is(err, errFetchDenied) {
				status = 400
			}
			if errors.Is(err, errFetchBusy) {
				status = 503
				code = "ERR_FETCH_BUSY"
			}
			if errors.Is(err, errFetchLarge) {
				status = 413
				code = httpx.CodeTooLarge
			}
			return httpx.Fail(c, status, code, err.Error())
		}
		c.Set("Content-Type", contentTypeBlob)
		c.Set("Content-Length", strconv.Itoa(len(data)))
		return c.Send(data)
	})
}
