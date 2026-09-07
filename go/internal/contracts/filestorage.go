package contracts

// The file storage types describe the JSON half of the file storage contract
// only. The bytes themselves never appear here: they cross the wire as a raw
// `application/octet-stream` body, which is the one place in `/api/**` where a
// success is not an envelope. See compat/phorge/README.md section 7.
//
// PhabricatorGorgeFileStorageEngine reads these field names directly, so they
// are the wire contract itself.

// WriteResult is the payload of a successful POST /api/file/blob.
//
// Engine matters as much as Handle: Phorge stores the pair, and a handle is
// only meaningful to the engine that minted it. The service picks the engine
// when the caller does not name one, so the answer has to say which one took
// the bytes.
type WriteResult struct {
	Handle string `json:"handle"`
	Engine string `json:"engine"`
	Size   int64  `json:"size"`
}

// DeleteResult is the payload of a successful DELETE /api/file/blob. Status is
// always "deleted", including when the object was already gone — the delete is
// idempotent, see docs/modules/file-storage.md section 3.4.
type DeleteResult struct {
	Status string `json:"status"`
}

// EngineInfo is one entry of GET /api/file/engines, in the order the router
// will try them for a write.
//
// SizeLimit is 0 for an engine that has none, which is why CanWrite is a
// separate field: "unlimited" and "refuses writes" would otherwise be the same
// value.
type EngineInfo struct {
	Identifier string `json:"identifier"`
	Priority   int    `json:"priority"`
	CanWrite   bool   `json:"canWrite"`
	SizeLimit  int64  `json:"sizeLimit"`
}
