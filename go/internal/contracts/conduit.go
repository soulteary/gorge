package contracts

// Conduit is the gateway domain, and it is the one wire contract in this
// repository that deliberately does NOT use the platform {data, error}
// envelope.
//
// gorge-conduit is a reverse proxy in front of Phorge's Conduit API
// (`ANY /api/:method`). On success it is a pass-through: the upstream's status,
// headers and body are relayed byte-for-byte, and Conduit's own reply is
// already the Conduit protocol envelope `{result, error_code, error_info}`.
// Wrapping that in {data, error} would corrupt a response every Conduit client
// (Phorge PHP, arcanist, other Go services) parses against the Conduit shape.
//
// The gateway therefore has to speak Conduit's language on the failures it
// raises itself too — an auth rejection, a rate-limit trip, an upstream that
// never answered, or a request missing the method segment. Answering those in
// the platform's {data, error} shape while relaying Conduit's own errors
// unchanged would hand callers two different error shapes from one endpoint.
// So ConduitError below is the failure envelope the gateway writes directly,
// bypassing httpx.OK/httpx.Fail. This is the same class of documented
// exception as file storage's raw-bytes success response; see
// docs/platform.md.
//
// The error codes are Conduit-style (hyphenated, ERR-CONDUIT-*), not the
// underscore ERR_* codes httpx defines, because they are part of a protocol
// that predates the monorepo and that clients already branch on.
type ConduitError struct {
	// Result is always null on an error. It mirrors Conduit's own successful
	// `result` field so a client can read one shape for both outcomes.
	Result any `json:"result"`
	// ErrorCode is the machine-readable failure code, e.g. ERR-CONDUIT-AUTH.
	ErrorCode string `json:"error_code"`
	// ErrorInfo is the human-readable description.
	ErrorInfo string `json:"error_info"`
}

// The gateway's own error codes. Upstream Conduit errors keep whatever code
// Phorge assigned them; these are only the ones the gateway mints before or
// instead of reaching the upstream.
const (
	// CodeConduitAuth is returned (401) when the service token is missing or
	// wrong.
	CodeConduitAuth = "ERR-CONDUIT-AUTH"
	// CodeRateLimit is returned (429) when a client trips the per-IP limiter.
	CodeRateLimit = "ERR-RATE-LIMIT"
	// CodeConduitProxy is returned (502) when the upstream request could not be
	// built or the upstream did not answer.
	CodeConduitProxy = "ERR-CONDUIT-PROXY"
	// CodeConduitCore is returned (400) when the URI carries no Conduit method.
	CodeConduitCore = "ERR-CONDUIT-CORE"
)

// NewConduitError builds an error envelope with a null result.
func NewConduitError(code, info string) *ConduitError {
	return &ConduitError{Result: nil, ErrorCode: code, ErrorInfo: info}
}
