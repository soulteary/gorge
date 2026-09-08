package dbapi

import (
	"errors"

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
	return false
}

func isRetryableQueryErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return retryableQueryCodes[myErr.Number]
	}
	return false
}

// classifyMySQLError turns a driver error into a domain DBError, choosing the
// kind from the errno. An access-denied maps to kindAccessDenied; anything
// else that is a MySQL error but not otherwise special is treated as an
// internal failure, since the caller cannot act on, say, a deadlock any
// differently than on a bug. A non-MySQL error (a dial failure) is left to the
// caller to wrap as kindUnreachable, which is what a failed connect means.
func classifyMySQLError(err error) *DBError {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		kind := kindInternal
		if accessDeniedCodes[myErr.Number] {
			kind = kindAccessDenied
		}
		return &DBError{Kind: kind, Message: err.Error(), Errno: myErr.Number}
	}
	return &DBError{Kind: kindInternal, Message: err.Error()}
}
