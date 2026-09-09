package dbapi

import "errors"

// This file is the deliberately small, documented bridge the migrated
// write-path package (internal/dbproxy) reaches through. Those abstractions —
// the master/replica Router, the savepoint TxManager and the write/connect
// retry helpers — were split out of this package because no HTTP handler or
// background task drives them (see docs/adr/0001-isolate-db-proxy.md), yet they
// still operate on this package's shared Conn/DSN/ClusterConfig/DatabaseRef and
// must classify failures with the same DBError taxonomy the read endpoints use.
//
// Rather than duplicate that taxonomy or export its internals wholesale, the
// proxy package consumes the handful of predicates and constructors below.
// Everything here is a thin, allocation-free wrapper over an existing internal
// symbol; production read paths keep using the unexported forms unchanged.

// IsReadQuery reports whether a statement is safe to run on a read-only
// connection (SELECT/SHOW/EXPLAIN). It backs both the shared Conn guard here
// and the transaction/retry guards in internal/dbproxy.
func IsReadQuery(query string) bool { return isReadQuery(query) }

// ClassifyMySQLError turns a driver error into the domain DBError the read
// endpoints answer with, so a write path in internal/dbproxy surfaces the same
// ERR_DB_ACCESS_DENIED / ERR_DB_UNREACHABLE distinctions.
func ClassifyMySQLError(err error) *DBError { return classifyMySQLError(err) }

// IsRetryableConnectErr reports whether a failed connect is worth another
// bounded attempt (timeout / refused / dropped TCP dial).
func IsRetryableConnectErr(err error) bool { return isRetryableConnectErr(err) }

// IsRetryableQueryErr reports whether a mid-query failure (dropped connection)
// is worth another attempt. Retrying is only safe for a read outside a
// transaction; the caller enforces that.
func IsRetryableQueryErr(err error) bool { return isRetryableQueryErr(err) }

// NewReadonlyError builds the ERR_READONLY DBError a write path returns when it
// refuses to write against a degraded or read-only target.
func NewReadonlyError(format string, args ...any) *DBError {
	return newDBError(kindReadonly, format, args...)
}

// NewUnreachableError builds the ERR_DB_UNREACHABLE DBError a write path
// returns when no reachable master exists for an application.
func NewUnreachableError(format string, args ...any) *DBError {
	return newDBError(kindUnreachable, format, args...)
}

// IsReadonlyError / IsAccessDeniedError let callers (and the proxy package's
// tests) recognise a classified failure without reaching the unexported kind
// constants. errors.As recovery is done here so a wrapped DBError still matches.
func IsReadonlyError(err error) bool     { return dbErrorKindIs(err, kindReadonly) }
func IsAccessDeniedError(err error) bool { return dbErrorKindIs(err, kindAccessDenied) }

func dbErrorKindIs(err error, kind errorKind) bool {
	var dbErr *DBError
	if errors.As(err, &dbErr) {
		return dbErr.Kind == kind
	}
	return false
}

// NewClusterConfigForRefs builds a ClusterConfig from an explicit set of refs,
// splitting them into masters and replicas by their IsMaster flag. The
// master/replica slices are unexported so a cluster is normally assembled by
// the file/scalar parsers in this package; this constructor exists so the
// out-of-package internal/dbproxy tests (which exercise the Router's
// master/replica selection) can stand up a cluster without reaching those
// unexported fields.
func NewClusterConfigForRefs(namespace, password string, refs []*DatabaseRef) *ClusterConfig {
	cc := &ClusterConfig{Refs: refs, Namespace: namespace, MySQLPass: password}
	for _, ref := range refs {
		if ref.IsMaster {
			cc.masters = append(cc.masters, ref)
		} else {
			cc.replicas = append(cc.replicas, ref)
		}
	}
	return cc
}
