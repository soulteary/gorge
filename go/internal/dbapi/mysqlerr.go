package dbapi

import (
	"errors"
	"net"

	"github.com/go-sql-driver/mysql"
)

// The MySQL error-code tables below are the domain algorithm preserved from
// the standalone service, which took them in turn from Phorge's
// AphrontBaseMySQLDatabaseConnection::throwCommonException. They decide two
// things: whether an operation is worth retrying, and how a failure is
// classified for the HTTP response.

// retryableConnectCodes are the connection-establishment errors a retry may
// clear: a timeout or a refused connection, both of which can be a database
// that is momentarily busy or still starting.
var retryableConnectCodes = map[uint16]bool{
	2002: true, // Connection Timeout
	2003: true, // Unable to Connect
}

// retryableQueryCodes are the mid-query errors a retry may clear: a connection
// dropped underneath a query. Retrying these is only safe for a read outside a
// transaction, a constraint QueryWithRetry enforces.
var retryableQueryCodes = map[uint16]bool{
	2013: true, // Connection Dropped
	2006: true, // Gone Away
}

// accessDeniedCodes are the privilege errors: the user is authenticated but
// lacks the grant the statement needs. These become ERR_DB_ACCESS_DENIED so a
// caller knows the fix is a GRANT, not a retry.
var accessDeniedCodes = map[uint16]bool{
	1044: true, // Access Denied (database)
	1142: true, // Access Denied (table)
	1143: true, // Access Denied (column)
	1227: true, // Access Denied (command, e.g. SHOW REPLICA STATUS)
	1045: true, // Wrong credentials
}

func isRetryableConnectErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return retryableConnectCodes[myErr.Number]
	}

	// database/sql and go-sql-driver/mysql return raw *net.OpError values for
	// real TCP dial failures (including refused connections and timeouts), not
	// the client-side 2002/2003 codes Phorge's PHP driver reports. They are the
	// same transient connection-establishment failures and should receive the
	// same bounded retry treatment.
	var netErr *net.OpError
	return errors.As(err, &netErr)
}

func isRetryableQueryErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return retryableQueryCodes[myErr.Number]
	}
	return false
}

// classifyMySQLError turns a driver error into a domain DBError, choosing the
// kind from the errno. An access-denied maps to kindAccessDenied, and a dropped
// connection maps to kindUnreachable. Other MySQL errors are internal since a
// caller cannot act on, say, a deadlock differently than on a bug. A non-MySQL
// query error is also a connection-level failure from database/sql (the lazy
// pool dials on first use), so it is classified as unreachable.
func classifyMySQLError(err error) *DBError {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		kind := kindInternal
		switch {
		case accessDeniedCodes[myErr.Number]:
			kind = kindAccessDenied
		case retryableQueryCodes[myErr.Number]:
			kind = kindUnreachable
		}
		return &DBError{Kind: kind, Message: err.Error(), Errno: myErr.Number}
	}
	return &DBError{Kind: kindUnreachable, Message: err.Error()}
}
