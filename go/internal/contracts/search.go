package contracts

// The search types are Phorge's view of a fulltext document, not this
// service's. PhabricatorGorgeFulltextStorageEngine serialises a
// PhabricatorSearchAbstractDocument straight into Document, and a
// PhabricatorSavedQuery straight into SearchQuery, so the camelCase field
// names below are the wire contract itself; see compat/phorge/README.md
// section 七.
//
// The four-character names inside a document — "titl", "cmnt", "auth", "ownr"
// and the rest — are not abbreviations this service chose. They are the values
// of PhabricatorSearchDocumentFieldType and PhabricatorSearchRelationship, and
// they reach the index as literal field names, so a document written with one
// spelling is unfindable by a query written with another. The constants live
// in go/internal/search/esquery/builder.go, the only place they are spelled
// out.

// Document is one indexable object, mirroring
// PhabricatorSearchAbstractDocument. It is the body of POST /api/search/index.
//
// Type is the PHID type ("TASK", "DREV", "CMIT", …) and doubles as the
// Elasticsearch mapping type, which is why an index has to be initialised for
// the document types a deployment actually indexes.
type Document struct {
	PHID          string             `json:"phid"`
	Type          string             `json:"type"`
	Title         string             `json:"title"`
	DateCreated   int64              `json:"dateCreated"`
	DateModified  int64              `json:"dateModified"`
	Fields        []DocumentField    `json:"fields,omitempty"`
	Relationships []DocumentRelation `json:"relationships,omitempty"`
}

// DocumentField is one (field_name, corpus, aux) tuple from
// PhabricatorSearchAbstractDocument::getFieldData().
//
// Name is a PhabricatorSearchDocumentFieldType value, so it is one of the
// four-character field names, not a human-readable label.
type DocumentField struct {
	Name   string `json:"name"`
	Corpus string `json:"corpus"`
	Aux    string `json:"aux,omitempty"`
}

// DocumentRelation is one (field_name, related_phid, rtype, time) tuple from
// PhabricatorSearchAbstractDocument::getRelationshipData().
//
// Name is a PhabricatorSearchRelationship value. Timestamp, when non-zero, is
// indexed under "<name>_ts", which is how the open and closed relationships
// carry the moment a document changed state.
type DocumentRelation struct {
	Name        string `json:"name"`
	RelatedPHID string `json:"relatedPHID"`
	RType       string `json:"rtype"`
	Timestamp   int64  `json:"timestamp,omitempty"`
}

// SearchQuery is the body of POST /api/search/query: the parameters of a
// PhabricatorSavedQuery, flattened.
//
// WithAnyOwner and WithUnowned are not redundant with OwnerPHIDs. Phorge's
// query UI has three separate states — "owned by these people", "owned by
// anyone" and "owned by nobody" — and the last two are expressed as the
// presence or absence of the ownr relationship rather than as a PHID list.
// WithAnyOwner also overrides OwnerPHIDs, matching the PHP query builder.
type SearchQuery struct {
	Query           string   `json:"query"`
	Types           []string `json:"types,omitempty"`
	AuthorPHIDs     []string `json:"authorPHIDs,omitempty"`
	OwnerPHIDs      []string `json:"ownerPHIDs,omitempty"`
	SubscriberPHIDs []string `json:"subscriberPHIDs,omitempty"`
	ProjectPHIDs    []string `json:"projectPHIDs,omitempty"`
	RepositoryPHIDs []string `json:"repositoryPHIDs,omitempty"`
	Statuses        []string `json:"statuses,omitempty"`
	WithAnyOwner    bool     `json:"withAnyOwner,omitempty"`
	WithUnowned     bool     `json:"withUnowned,omitempty"`
	Exclude         string   `json:"exclude,omitempty"`
	Offset          int      `json:"offset,omitempty"`
	Limit           int      `json:"limit,omitempty"`
}

// DocTypesRequest is the body of both POST /api/search/init and
// POST /api/search/sane. Both need the list because an index's mapping is
// built per document type, and both reject an empty one: an init with no types
// creates an index nothing can be written to, and a sanity check with no types
// compares an empty expectation against anything at all and answers true.
type DocTypesRequest struct {
	DocTypes []string `json:"docTypes"`
}

// IndexAck is the data of POST /api/search/index. It echoes the PHID so a
// caller batching documents can tell which one an answer belongs to.
type IndexAck struct {
	PHID   string `json:"phid"`
	Status string `json:"status"`
}

// SearchResults is the data of POST /api/search/query.
//
// PHIDs is the whole answer: the service returns identifiers, never document
// bodies. Phorge loads the objects itself through its own query classes, which
// is what keeps its policy checks in the loop — a search backend that returned
// content would be handing out objects the viewer may not be allowed to see.
//
// Count is len(PHIDs) for this page, not a total hit count.
type SearchResults struct {
	PHIDs []string `json:"phids"`
	Count int      `json:"count"`
}

// InitAck is the data of POST /api/search/init.
type InitAck struct {
	Status string `json:"status"`
}

// IndexPresence is the data of GET /api/search/exists.
type IndexPresence struct {
	Exists bool `json:"exists"`
}

// IndexSanity is the data of POST /api/search/sane: whether the live index
// configuration still matches the one this service would create today.
//
// False is the normal answer after this service changes a mapping, and it is
// the designed signal that a reindex is due — see docs/modules/search.md
// section 3.3 for the CJK subfield, which is exactly such a change.
type IndexSanity struct {
	Sane bool `json:"sane"`
}

// IndexStats is the data of GET /api/search/stats. It is an open map rather
// than a struct because the keys are the backend's, not this service's:
// Elasticsearch reports queries/documents/deleted/storage_bytes while
// Meilisearch reports documents/indexing. A struct would have to declare the
// union and would then quietly answer zero for whichever half is missing.
//
// "storage_bytes" is the one snake_case name on this service's wire. It is
// kept because it predates the monorepo and the PHP side reads it by that
// spelling; renaming it to storageBytes leaves the cluster panel's storage
// column blank without producing any error.
type IndexStats map[string]any

// BackendInfo is one entry of GET /api/search/backends. It is an open map for
// the same reason as IndexStats, and for one more: the two backends do not
// even agree on their host key. Elasticsearch reports "hosts" as a list,
// because it fans out and keeps a per-host health table; Meilisearch reports a
// single "host" string. Both always carry "type", "index" and "roles".
//
// Credentials never appear here.
type BackendInfo map[string]any
