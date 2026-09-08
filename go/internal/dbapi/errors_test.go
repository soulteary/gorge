package dbapi

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// The error tests are ported from the standalone service's
// internal/compat/errors_test.go. The standalone service mapped a MySQL errno
// to its own thirteen-code taxonomy and then to an HTTP status; this package
// collapses that to the platform six plus three domain codes, so the tests are
// rewritten to assert the collapse: which errno becomes which domain kind, and
// which kind answers which wire code and status.

// TestCodeForKind pins the domain-kind → (wire code, HTTP status) mapping. The
// three caller-actionable kinds each keep a code; everything else is left to
// the platform handler as a 500 ERR_INTERNAL, which is what ok == false means.
func TestCodeForKind(t *testing.T) {
	cases := []struct {
		kind       errorKind
		wantCode   string
		wantStatus int
		wantOK     bool
	}{
		{kindUnreachable, CodeDBUnreachable, http.StatusServiceUnavailable, true},
		{kindReadonly, CodeReadonly, http.StatusConflict, true},
		{kindAccessDenied, CodeAccessDenied, http.StatusForbidden, true},
		{kindInternal, "", 0, false},
	}
	for _, tc := range cases {
		code, status, ok := codeForKind(tc.kind)
		if ok != tc.wantOK {
			t.Errorf("codeForKind(%d) ok = %v, want %v", tc.kind, ok, tc.wantOK)
		}
		if code != tc.wantCode {
			t.Errorf("codeForKind(%d) code = %q, want %q", tc.kind, code, tc.wantCode)
		}
		if status != tc.wantStatus {
			t.Errorf("codeForKind(%d) status = %d, want %d", tc.kind, status, tc.wantStatus)
		}
	}
}

// TestDomainCodeConstants pins the wire spelling of the three domain codes.
// PhabricatorGorgeDBClient branches on these strings.
func TestDomainCodeConstants(t *testing.T) {
	cases := map[string]string{
		CodeDBUnreachable: "ERR_DB_UNREACHABLE",
		CodeReadonly:      "ERR_READONLY",
		CodeAccessDenied:  "ERR_DB_ACCESS_DENIED",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("domain code = %q, want %q", got, want)
		}
	}
}

// TestDomainCodesAreNotThePlatformCodes: the three domain codes must be
// distinct from the platform six, since they are defined here precisely to
// carry a distinction ERR_INTERNAL would throw away.
func TestDomainCodesAreNotThePlatformCodes(t *testing.T) {
	platform := map[string]bool{
		httpx.CodeBadRequest:       true,
		httpx.CodeUnauthorized:     true,
		httpx.CodeNotFound:         true,
		httpx.CodeMethodNotAllowed: true,
		httpx.CodeTooLarge:         true,
		httpx.CodeInternal:         true,
	}
	for _, code := range []string{CodeDBUnreachable, CodeReadonly, CodeAccessDenied} {
		if platform[code] {
			t.Errorf("domain code %q collides with a platform code", code)
		}
	}
}

// TestClassifyMySQLErrorAccessDenied: the five access-denied errnos map to
// kindAccessDenied so a caller learns the fix is a GRANT rather than a retry.
func TestClassifyMySQLErrorAccessDenied(t *testing.T) {
	for _, errno := range []uint16{1044, 1142, 1143, 1227, 1045} {
		err := classifyMySQLError(&mysql.MySQLError{Number: errno, Message: "denied"})
		if err.Kind != kindAccessDenied {
			t.Errorf("errno %d: kind = %d, want kindAccessDenied", errno, err.Kind)
		}
		if err.Errno != errno {
			t.Errorf("errno %d: preserved errno = %d", errno, err.Errno)
		}
	}
}

// TestClassifyMySQLErrorOther: any other MySQL errno is not caller-actionable
// and becomes kindInternal — a deadlock or a duplicate key is a bug or a
// conflict the caller cannot act on differently.
func TestClassifyMySQLErrorOther(t *testing.T) {
	for _, errno := range []uint16{1213, 1205, 1062, 1146, 9999} {
		err := classifyMySQLError(&mysql.MySQLError{Number: errno, Message: "x"})
		if err.Kind != kindInternal {
			t.Errorf("errno %d: kind = %d, want kindInternal", errno, err.Kind)
		}
	}
}

func TestClassifyMySQLConnectionLossAsUnreachable(t *testing.T) {
	for _, errno := range []uint16{2006, 2013} {
		err := classifyMySQLError(&mysql.MySQLError{Number: errno, Message: "connection lost"})
		if err.Kind != kindUnreachable {
			t.Errorf("errno %d: kind = %d, want kindUnreachable", errno, err.Kind)
		}
		if err.Errno != errno {
			t.Errorf("errno %d: preserved errno = %d", errno, err.Errno)
		}
	}
}

// TestClassifyMySQLErrorNonDriver: database/sql dials lazily, so a non-driver
// query error is a connection-level failure and must be caller-actionable.
func TestClassifyMySQLErrorNonDriver(t *testing.T) {
	err := classifyMySQLError(errors.New("connection refused"))
	if err.Kind != kindUnreachable {
		t.Errorf("kind = %d, want kindUnreachable", err.Kind)
	}
	if err.Errno != 0 {
		t.Errorf("errno = %d, want 0", err.Errno)
	}
}

func TestIsRetryableConnectErr(t *testing.T) {
	for _, errno := range []uint16{2002, 2003} {
		if !isRetryableConnectErr(&mysql.MySQLError{Number: errno}) {
			t.Errorf("errno %d should be a retryable connect error", errno)
		}
	}
	for _, errno := range []uint16{2013, 1044, 1062} {
		if isRetryableConnectErr(&mysql.MySQLError{Number: errno}) {
			t.Errorf("errno %d should NOT be a retryable connect error", errno)
		}
	}
	for _, err := range []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{IsTimeout: true}},
	} {
		if !isRetryableConnectErr(err) {
			t.Errorf("%T should be a retryable connect error", err)
		}
	}
	if isRetryableConnectErr(errors.New("plain error")) {
		t.Error("a non-driver error is not a retryable connect error")
	}
}

func TestIsRetryableQueryErr(t *testing.T) {
	for _, errno := range []uint16{2013, 2006} {
		if !isRetryableQueryErr(&mysql.MySQLError{Number: errno}) {
			t.Errorf("errno %d should be a retryable query error", errno)
		}
	}
	for _, errno := range []uint16{2002, 1044} {
		if isRetryableQueryErr(&mysql.MySQLError{Number: errno}) {
			t.Errorf("errno %d should NOT be a retryable query error", errno)
		}
	}
}

// TestGenericMessageLeaksNothing: the message a domain kind answers with names
// the kind of failure without a host, database or query. This is the safe
// half of codeForKind — the detail is logged, not returned.
func TestGenericMessageLeaksNothing(t *testing.T) {
	for _, kind := range []errorKind{kindUnreachable, kindReadonly, kindAccessDenied, kindInternal} {
		msg := genericMessage(kind)
		if msg == "" {
			t.Errorf("kind %d has an empty generic message", kind)
		}
		for _, leak := range []string{"3306", "phorge", "SELECT", "127.0.0.1", "meta_data"} {
			if strings.Contains(msg, leak) {
				t.Errorf("generic message %q leaks %q", msg, leak)
			}
		}
	}
	if genericMessage(kindInternal) != http.StatusText(http.StatusInternalServerError) {
		t.Errorf("internal kind should answer the standard 500 text")
	}
}

// TestDBErrorError pins the log string: it carries the errno when there is one
// and does not invent a # when there is not.
func TestDBErrorError(t *testing.T) {
	withErrno := &DBError{Kind: kindAccessDenied, Message: "denied", Errno: 1044}
	if s := withErrno.Error(); !strings.Contains(s, "#1044") || !strings.Contains(s, "denied") {
		t.Errorf("error string = %q, want the errno and message", s)
	}
	noErrno := &DBError{Kind: kindReadonly, Message: "read only"}
	if s := noErrno.Error(); strings.Contains(s, "#") {
		t.Errorf("error string %q should not carry a # without an errno", s)
	}
}

// TestDBErrorIsAnError: DBError satisfies the error interface and is matchable
// with errors.As, which is how the HTTP layer's fail() recovers the kind.
func TestDBErrorIsAnError(t *testing.T) {
	var err error = newDBError(kindUnreachable, "server %q down", "db1")
	var dbErr *DBError
	if !errors.As(err, &dbErr) {
		t.Fatal("a DBError should be recoverable with errors.As")
	}
	if dbErr.Kind != kindUnreachable {
		t.Errorf("kind = %d, want kindUnreachable", dbErr.Kind)
	}
}
