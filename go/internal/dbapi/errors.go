package dbapi

import (
	"fmt"
	"net/http"
)

// The db-api domain defines three error codes of its own, alongside the
// platform six (ERR_BAD_REQUEST, ERR_UNAUTHORIZED, ERR_NOT_FOUND,
// ERR_METHOD_NOT_ALLOWED, ERR_TOO_LARGE, ERR_INTERNAL from platform/httpx).
//
// These three exist for the same reason file-storage's ERR_NO_ENGINE does: they
// are the failures a *caller* — a Phorge setup check, the PHP client — needs to
// tell apart and act on, and collapsing them into ERR_INTERNAL would throw that
// distinction away. A database that is merely unreachable is a transient state
// an operator waits out or fixes in the infrastructure; a read-only conflict is
// something the caller retries against a master; an access-denied is a GRANT to
// add. Each is a different remedy, so each keeps a code.
//
// Everything else — a malformed query, an unknown route, a bug — maps to the
// platform codes. The domain codes are defined here and NOT in platform/httpx,
// which owns only the codes every service shares.
const (
	// CodeDBUnreachable: the target server (or every server) could not be
	// reached. 503, because nothing is broken in this service — the database
	// is down or not up yet.
	CodeDBUnreachable = "ERR_DB_UNREACHABLE"

	// CodeReadonly: a write was asked of a connection or router that is in
	// read-only mode, which happens when the master is unreachable and the
	// service has degraded to serving reads from a replica. 409, because the
	// caller can resolve it by directing the write at a reachable master.
	CodeReadonly = "ERR_READONLY"

	// CodeAccessDenied: the configured database user lacks the privilege the
	// operation needs. 403, because it is an authorization problem on the
	// database, distinct from this service's own token check (401).
	CodeAccessDenied = "ERR_DB_ACCESS_DENIED"
)

// domainCode maps one of this domain's internal error kinds to the wire code
// and HTTP status a handler answers with. A kind with no entry here is not a
// caller-actionable failure and is left to the platform error handler as a
// 500 ERR_INTERNAL.
//
// The message is deliberately not carried through: a body that leaked a host
// name, a database name or a fragment of SQL would hand an unauthenticated
// probe (the token check having already passed for a service-to-service call)
// a map of the cluster. Handlers answer these codes with a generic message and
// log the detail. See compat/phorge/README.md.
func codeForKind(kind errorKind) (code string, status int, ok bool) {
	switch kind {
	case kindUnreachable:
		return CodeDBUnreachable, http.StatusServiceUnavailable, true
	case kindReadonly:
		return CodeReadonly, http.StatusConflict, true
	case kindAccessDenied:
		return CodeAccessDenied, http.StatusForbidden, true
	default:
		return "", 0, false
	}
}

// errorKind classifies a domain failure into one of the caller-actionable
// buckets above, or kindInternal for everything else.
type errorKind int

const (
	kindInternal errorKind = iota
	kindUnreachable
	kindReadonly
	kindAccessDenied
)

// DBError is the domain's internal error. It carries a kind for the HTTP
// mapping and an optional MySQL errno for the log, but never reaches the
// response body verbatim — see codeForKind.
type DBError struct {
	Kind    errorKind
	Message string
	Errno   uint16
}

func (e *DBError) Error() string {
	if e.Errno > 0 {
		return fmt.Sprintf("dbapi: #%d: %s", e.Errno, e.Message)
	}
	return "dbapi: " + e.Message
}

func newDBError(kind errorKind, format string, args ...any) *DBError {
	return &DBError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}
