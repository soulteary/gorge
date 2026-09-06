package contracts

// DiffRequest is the body of POST /api/diff/generate. Empty names default to
// /dev/universe, the placeholder Phorge uses for content that has no path.
// Normalize strips spaces and tabs from both sides before comparing, matching
// PhabricatorDifferenceEngine's normalize mode.
type DiffRequest struct {
	Old       string `json:"old"`
	New       string `json:"new"`
	OldName   string `json:"oldName,omitempty"`
	NewName   string `json:"newName,omitempty"`
	Normalize bool   `json:"normalize,omitempty"`
}

// DiffResult is the payload returned inside the response envelope's data field.
// Diff is a complete unified diff, headers included, byte-compatible with the
// `diff -U65535` output ArcanistDiffParser expects. Equal reports that the two
// sides matched, in which case Diff carries the changeless diff Phorge renders
// the unchanged file from.
type DiffResult struct {
	Diff  string `json:"diff"`
	Equal bool   `json:"equal"`
}

// ProseRequest is the body of POST /api/diff/prose.
type ProseRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ProsePart is one segment of a prose diff. Type is "=", "-" or "+", the same
// three markers PhutilProseDiff uses. Concatenating the "=" and "-" segments
// reproduces the old text; "=" and "+" reproduce the new one.
type ProsePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ProseResult is the payload returned inside the response envelope's data field.
type ProseResult struct {
	Parts []ProsePart `json:"parts"`
}
