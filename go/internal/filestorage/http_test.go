package filestorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

// newTestServer builds the routes the way cmd/gorge-file-storage does, so the
// platform error handler and the health probes are in play.
func newTestServer(t *testing.T, engines ...StorageEngine) *echo.Echo {
	t.Helper()
	quietLogs(t)

	router := NewRouter(engines)
	srv := httpx.New(httpx.Config{
		BodyLimit: TransportBodyLimit,
		Ready:     router.Ready,
	})
	RegisterRoutes(srv.Echo(), &Deps{Router: router, Token: testToken})
	return srv.Echo()
}

// do issues an authenticated request. A nil body sends none at all, which is
// distinct from sending an empty one.
func do(e *echo.Echo, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("X-Service-Token", testToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

type testEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) testEnvelope {
	t.Helper()

	var decoded testEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not an envelope: %v (body: %s)", err, rec.Body.String())
	}
	return decoded
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("expected %d, got %d (body: %s)", status, rec.Code, rec.Body.String())
	}
	env := envelope(t, rec)
	if env.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", rec.Body.String())
	}
	if env.Error.Code != code {
		t.Errorf("expected %s, got %s", code, env.Error.Code)
	}
	if len(env.Data) != 0 {
		t.Errorf("an error response must carry no data, got: %s", env.Data)
	}
}

func TestWriteBlob(t *testing.T) {
	disk := diskLike()
	e := newTestServer(t, blobLike(1000), disk)

	rec := do(e, http.MethodPost, "/api/file/blob", strings.NewReader("hello gorge"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The JSON half of the contract: a write answers the envelope, because
	// what it has to report is metadata, not bytes.
	var result struct {
		Handle string `json:"handle"`
		Engine string `json:"engine"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(envelope(t, rec).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Engine != "blob" {
		t.Errorf("engine = %q, want the lowest-priority engine that fits", result.Engine)
	}
	if result.Size != 11 || result.Handle == "" {
		t.Errorf("unexpected result: %+v", result)
	}
}

// TestWriteBlobRequiresAContentLength: a body of unknown length cannot be
// signed for S3 nor checked against a backend's size limit before it is read.
// Refusing it is better than silently buffering it.
func TestWriteBlobRequiresAContentLength(t *testing.T) {
	e := newTestServer(t, diskLike())

	// httptest.NewRequest only derives a ContentLength for the reader types it
	// recognises, so an opaque reader produces the -1 a chunked upload would.
	req := httptest.NewRequest(http.MethodPost, "/api/file/blob", oneByteReader{strings.NewReader("x")})
	req.Header.Set("X-Service-Token", testToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assertErrorCode(t, rec, http.StatusBadRequest, httpx.CodeBadRequest)
}

func TestWriteBlobAcceptsAnEmptyFile(t *testing.T) {
	e := newTestServer(t, diskLike())

	// Zero bytes with a Content-Length of 0 is a real file, not a missing
	// body. Phorge stores empty files.
	rec := do(e, http.MethodPost, "/api/file/blob", strings.NewReader(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"size":0`) {
		t.Errorf("expected size 0, got %s", rec.Body.String())
	}
}

func TestWriteBlobToANamedEngine(t *testing.T) {
	e := newTestServer(t, blobLike(1000), diskLike())

	// Naming an engine overrides the priority order: Phorge does it when the
	// file's engine is already recorded against it.
	rec := do(e, http.MethodPost, "/api/file/blob?engine=local-disk", strings.NewReader("hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"engine":"local-disk"`) {
		t.Errorf("expected the named engine, got %s", rec.Body.String())
	}
}

func TestWriteBlobUnknownEngineIs400(t *testing.T) {
	e := newTestServer(t, diskLike())

	// The caller's mistake, not a server failure: a handle is meaningless
	// without the engine that minted it, so there is nothing to substitute.
	rec := do(e, http.MethodPost, "/api/file/blob?engine=nope", strings.NewReader("hello"))
	assertErrorCode(t, rec, http.StatusBadRequest, httpx.CodeBadRequest)
}

// TestWriteBlobOverANamedEnginesLimitIs413: reported as a size refusal like
// every other one in the repository, rather than as the 500 the engine's own
// error would have become.
func TestWriteBlobOverANamedEnginesLimitIs413(t *testing.T) {
	e := newTestServer(t, blobLike(10), diskLike())

	rec := do(e, http.MethodPost, "/api/file/blob?engine=blob",
		strings.NewReader(strings.Repeat("x", 500)))
	assertErrorCode(t, rec, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge)
}

// TestWriteBlobNoEngineIs503: this domain's only error code. It says nothing
// was attempted, which is what makes it different from a write that failed —
// and it is a configuration problem a Phorge setup check can act on.
func TestWriteBlobNoEngineIs503(t *testing.T) {
	e := newTestServer(t)

	rec := do(e, http.MethodPost, "/api/file/blob", strings.NewReader("hello"))
	assertErrorCode(t, rec, http.StatusServiceUnavailable, CodeNoEngine)
}

func TestWriteBlobEveryEngineFailingIs500(t *testing.T) {
	disk := diskLike()
	disk.writeErr = errors.New("disk full")
	e := newTestServer(t, disk)

	rec := do(e, http.MethodPost, "/api/file/blob", strings.NewReader("hello"))
	// Not ERR_NO_ENGINE: a backend is configured and it broke. The engine's
	// own error stays in the log, since a 5xx body never describes internals.
	assertErrorCode(t, rec, http.StatusInternalServerError, httpx.CodeInternal)
}

// TestWriteBlobFallsThroughToTheNextEngine is the router's fall-through seen
// from the outside: the caller gets a 200 naming the engine that actually took
// the file.
func TestWriteBlobFallsThroughToTheNextEngine(t *testing.T) {
	blob := blobLike(1000)
	blob.writeErr = errors.New("Table 'phorge_file.file_storageblob' doesn't exist")
	blob.drainFirst = true
	disk := diskLike()

	e := newTestServer(t, blob, disk)

	rec := do(e, http.MethodPost, "/api/file/blob", strings.NewReader("hello gorge"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"engine":"local-disk"`) {
		t.Errorf("expected the fallback engine to be reported, got %s", rec.Body.String())
	}
}

// TestReadBlobAnswersRawBytes is the contract this domain introduced: the only
// success response in /api/** that is not the {data, error} envelope.
func TestReadBlobAnswersRawBytes(t *testing.T) {
	disk := diskLike()
	disk.seed("seeded", []byte("hello gorge"))
	e := newTestServer(t, disk)

	rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle=seeded", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello gorge" {
		t.Errorf("body = %q, want the file's bytes and nothing else", got)
	}
	// The Content-Type is what the PHP client branches on to tell the binary
	// shape from the envelope.
	if got := rec.Header().Get(echo.HeaderContentType); got != contentTypeBlob {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeBlob)
	}
	// The Content-Length is what lets the client tell a complete file from a
	// truncated one.
	if got := rec.Header().Get(echo.HeaderContentLength); got != "11" {
		t.Errorf("Content-Length = %q, want 11", got)
	}
	if strings.Contains(rec.Body.String(), `"data"`) {
		t.Error("a successful read must not be wrapped in the envelope")
	}
}

// TestReadBlobAnswersAnEmptyFile: an empty body with status 200 is a real
// answer, which is why the PHP client has to branch on the status code rather
// than on whether the body is empty.
func TestReadBlobAnswersAnEmptyFile(t *testing.T) {
	disk := diskLike()
	disk.seed("empty", []byte{})
	e := newTestServer(t, disk)

	rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle=empty", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected an empty body, got %q", rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderContentLength); got != "0" {
		t.Errorf("Content-Length = %q, want 0", got)
	}
}

// TestReadBlobOmitsContentLengthWhenTheSizeIsUnknown: the header is only
// honest when the engine knows the length. Guessing it would be worse than
// leaving the response chunked.
func TestReadBlobOmitsContentLengthWhenTheSizeIsUnknown(t *testing.T) {
	disk := diskLike()
	disk.sizeUnknown = true
	disk.seed("seeded", []byte("hello gorge"))
	e := newTestServer(t, disk)

	rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle=seeded", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get(echo.HeaderContentLength); got != "" {
		t.Errorf("Content-Length = %q, want it absent", got)
	}
	if rec.Body.String() != "hello gorge" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestReadBlobFailureKeepsTheEnvelope is the other half of the mixed contract:
// success is bytes, failure is still JSON.
func TestReadBlobFailureKeepsTheEnvelope(t *testing.T) {
	e := newTestServer(t, diskLike())

	rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle=gone", nil)
	assertErrorCode(t, rec, http.StatusNotFound, httpx.CodeNotFound)
}

func TestReadBlobRequiresBothParameters(t *testing.T) {
	e := newTestServer(t, diskLike())

	for _, path := range []string{
		"/api/file/blob",
		"/api/file/blob?engine=local-disk",
		"/api/file/blob?handle=seeded",
	} {
		t.Run(path, func(t *testing.T) {
			// The engine is required rather than inferred: handle formats
			// overlap between backends, so guessing would sometimes read the
			// wrong file rather than fail.
			assertErrorCode(t, do(e, http.MethodGet, path, nil),
				http.StatusBadRequest, httpx.CodeBadRequest)
		})
	}
}

// TestHandlesWithSlashesSurviveTheQueryString is why the handle is a query
// parameter and not a path segment: a local disk handle is `ab/cd/{28 hex}`.
//
// Both spellings of the slash are exercised because the two real callers do
// not agree on one. PhutilURI percent-encodes it, so PhabricatorGorge-
// FileStorageClient sends `%2F`, while curl and tests/e2e/file-storage.sh send
// the byte as-is. Both are legal in a query string and both must reach the
// engine as the same handle; testing only the raw form would leave the shape
// Phorge actually sends unguarded.
func TestHandlesWithSlashesSurviveTheQueryString(t *testing.T) {
	const handle = "ab/cd/0123456789abcdef0123456789ab"

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"raw slashes", handle},
		{"percent-encoded slashes", strings.ReplaceAll(handle, "/", "%2F")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk := diskLike()
			disk.seed(handle, []byte("hello gorge"))
			e := newTestServer(t, disk)

			rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle="+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != "hello gorge" {
				t.Errorf("body = %q", rec.Body.String())
			}
		})
	}
}

func TestDeleteBlob(t *testing.T) {
	disk := diskLike()
	disk.seed("seeded", []byte("hello gorge"))
	e := newTestServer(t, disk)

	rec := do(e, http.MethodDelete, "/api/file/blob?engine=local-disk&handle=seeded", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Errorf("unexpected response: %s", rec.Body.String())
	}
	if _, ok := disk.object("seeded"); ok {
		t.Error("the object is still there")
	}
}

// TestDeleteBlobIsIdempotent: Phorge deletes the bytes and the row pointing at
// them in one sequence, so an error for bytes that are already gone would
// leave it unable to retire the row.
func TestDeleteBlobIsIdempotent(t *testing.T) {
	e := newTestServer(t, diskLike())

	rec := do(e, http.MethodDelete, "/api/file/blob?engine=local-disk&handle=gone", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteBlobFailureIs500(t *testing.T) {
	disk := diskLike()
	disk.deleteErr = errors.New("permission denied")
	e := newTestServer(t, disk)

	rec := do(e, http.MethodDelete, "/api/file/blob?engine=local-disk&handle=seeded", nil)
	assertErrorCode(t, rec, http.StatusInternalServerError, httpx.CodeInternal)
}

// The other half of the split, and the reason ErrBadHandle exists: a delete
// reports an already-gone object as success, so without the distinction every
// remaining failure is a 500 and a caller sending nonsense is told the service
// is broken. A real backend fault must stay a 500 — the test above.
func TestDeleteBlobBadHandleIs400(t *testing.T) {
	disk := diskLike()
	disk.deleteErr = fmt.Errorf("malformed handle %q: %w", "../../etc/passwd", ErrBadHandle)
	e := newTestServer(t, disk)

	rec := do(e, http.MethodDelete, "/api/file/blob?engine=local-disk&handle=../../etc/passwd", nil)
	assertErrorCode(t, rec, http.StatusBadRequest, httpx.CodeBadRequest)
}

// Read keeps answering 404 for the same handle, deliberately: the caller holds
// one engine/handle pair and cannot act on the difference. Pinned so the
// asymmetry with delete stays a decision rather than a regression.
func TestReadBlobBadHandleStays404(t *testing.T) {
	root := t.TempDir()
	eng, err := NewLocalDiskEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestServer(t, eng)

	rec := do(e, http.MethodGet, "/api/file/blob?engine=local-disk&handle=../../etc/passwd", nil)
	assertErrorCode(t, rec, http.StatusNotFound, httpx.CodeNotFound)
}

// And the engines really do report it, which the two handler tests above stub
// past. Local disk is the one where the validation is also a security
// boundary: filepath.Join resolves the `..` if the pattern does not reject it.
func TestEnginesReportABadHandle(t *testing.T) {
	root := t.TempDir()
	disk, err := NewLocalDiskEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.DeleteFile(context.Background(), "../../etc/passwd"); !errors.Is(err, ErrBadHandle) {
		t.Errorf("local disk delete: got %v, want ErrBadHandle", err)
	}
	if _, _, err := disk.ReadFile(context.Background(), "../../etc/passwd"); !errors.Is(err, ErrBadHandle) {
		t.Errorf("local disk read: got %v, want ErrBadHandle", err)
	}

	blob := newBlobEngine(t, 1000)
	if err := blob.DeleteFile(context.Background(), "not-a-row-id"); !errors.Is(err, ErrBadHandle) {
		t.Errorf("blob delete: got %v, want ErrBadHandle", err)
	}
	if _, _, err := blob.ReadFile(context.Background(), "not-a-row-id"); !errors.Is(err, ErrBadHandle) {
		t.Errorf("blob read: got %v, want ErrBadHandle", err)
	}
}

func TestListEngines(t *testing.T) {
	e := newTestServer(t, diskLike(), blobLike(1000))

	rec := do(e, http.MethodGet, "/api/file/engines", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var info []struct {
		Identifier string `json:"identifier"`
		Priority   int    `json:"priority"`
		CanWrite   bool   `json:"canWrite"`
		SizeLimit  int64  `json:"sizeLimit"`
	}
	if err := json.Unmarshal(envelope(t, rec).Data, &info); err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 {
		t.Fatalf("expected 2 engines, got %+v", info)
	}
	// Reported in the order a write tries them, so a deployment's routing is
	// verifiable from outside. Phorge's setup check reads this endpoint.
	if info[0].Identifier != "blob" || info[1].Identifier != "local-disk" {
		t.Errorf("expected priority order, got %+v", info)
	}
	if info[0].SizeLimit != 1000 || !info[0].CanWrite {
		t.Errorf("unexpected blob entry: %+v", info[0])
	}
}

func TestTokenAuth(t *testing.T) {
	e := newTestServer(t, diskLike())

	t.Run("rejected without a token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/file/engines", nil))
		assertErrorCode(t, rec, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("accepted in the header", func(t *testing.T) {
		if rec := do(e, http.MethodGet, "/api/file/engines", nil); rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("accepted in the query string", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/file/engines?token="+testToken, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a read is guarded too", func(t *testing.T) {
		// The binary endpoint is the one where an unguarded route would leak
		// file contents rather than metadata.
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/api/file/blob?engine=local-disk&handle=seeded", nil))
		assertErrorCode(t, rec, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})
}

// TestReadyzReportsUnconfiguredBackends is the probe half of the readiness
// contract: the process is alive, but it can store nothing, and orchestration
// has to be able to tell those apart.
func TestReadyzReportsUnconfiguredBackends(t *testing.T) {
	probe := func(e *echo.Echo, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	unconfigured := newTestServer(t)

	if rec := probe(unconfigured, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness must not depend on the backends, got %d", rec.Code)
	}

	rec := probe(unconfigured, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	// Probe payloads stay outside the {data,error} envelope: orchestrators are
	// configured against the flat shape.
	if strings.Contains(rec.Body.String(), `"data"`) {
		t.Errorf("probe payload must not use the envelope: %s", rec.Body.String())
	}

	if rec := probe(newTestServer(t, diskLike()), "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("expected 200 once a backend is configured, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRoutePathsAreStable: PhabricatorGorgeFileStorageClient calls these paths
// as written, so renaming any of them is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	e := newTestServer(t, diskLike())

	want := map[string]string{
		"POST /api/file/blob":   "",
		"GET /api/file/blob":    "",
		"DELETE /api/file/blob": "",
		"GET /api/file/engines": "",
		"GET /healthz":          "",
		"GET /readyz":           "",
	}
	for _, r := range e.Routes() {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

// TestUnknownPathKeepsTheEnvelope: the binary response is a property of one
// handler, not of the port. Everything the framework answers on its own is
// still an envelope, which is what the PHP client falls back to reading.
func TestUnknownPathKeepsTheEnvelope(t *testing.T) {
	e := newTestServer(t, diskLike())

	rec := do(e, http.MethodGet, "/api/file/nope", nil)
	assertErrorCode(t, rec, http.StatusNotFound, httpx.CodeNotFound)
}
