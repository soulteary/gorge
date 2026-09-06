package contracts

// The notification admin port is the one place in this repository whose
// *successful* responses are deliberately not wrapped in the httpx
// {data,error} envelope. Phorge's PhabricatorNotificationServerRef decodes
// these bodies with a bare phutil_json_decode() and indexes the keys directly,
// so an envelope puts every field out of reach without any error being raised.
// Failures still answer in the envelope: the PHP client calls resolvex(), which
// throws on a non-2xx without ever parsing the body. See
// compat/phorge/README.md.

// AphlictReceipt is the body of a successful POST / on the admin port. The
// fingerprint identifies the server that accepted the message, which is how a
// cluster peer recognises a message that has already passed through it.
type AphlictReceipt struct {
	Fingerprint string `json:"fingerprint"`
}

// AphlictStatus is the body of GET /status/ on the admin port.
//
// The dots in these keys are literal, not a nesting convention: Phorge's
// cluster notification panel reads them as flat keys — idx($details,
// 'clients.active') — so turning them into nested objects would leave the panel
// with nothing to show. Uptime and HistoryAge are milliseconds. HistoryAge is a
// pointer because an empty history reports null; Phorge only reads it when
// HistorySize is non-zero.
type AphlictStatus struct {
	Instance      string `json:"instance"`
	Uptime        int64  `json:"uptime"`
	ClientsActive int    `json:"clients.active"`
	ClientsTotal  int64  `json:"clients.total"`
	MessagesIn    int64  `json:"messages.in"`
	MessagesOut   int64  `json:"messages.out"`
	HistorySize   int    `json:"history.size"`
	HistoryAge    *int64 `json:"history.age"`
	Version       int    `json:"version"`
}
