// Package esquery assembles Elasticsearch bool queries and holds the field
// names Phorge indexes documents under.
//
// It is a package of its own rather than part of the elasticsearch backend
// because the four-character names below are Phorge's, and the Meilisearch
// backend has to spell them exactly the same way. Two copies would drift, and
// the drift would be invisible: documents written under one spelling are
// simply never found by a query written under the other.
package esquery

// BoolQuery builds an Elasticsearch bool query clause by clause, mirroring the
// PHP PhabricatorElasticsearchQueryBuilder.
type BoolQuery struct {
	Must    []any `json:"must,omitempty"`
	Should  []any `json:"should,omitempty"`
	Filter  []any `json:"filter,omitempty"`
	MustNot []any `json:"must_not,omitempty"`
}

func (q *BoolQuery) AddMust(clause any)    { q.Must = append(q.Must, clause) }
func (q *BoolQuery) AddShould(clause any)  { q.Should = append(q.Should, clause) }
func (q *BoolQuery) AddFilter(clause any)  { q.Filter = append(q.Filter, clause) }
func (q *BoolQuery) AddMustNot(clause any) { q.MustNot = append(q.MustNot, clause) }

func (q *BoolQuery) AddExists(field string) {
	q.AddFilter(map[string]any{
		"exists": map[string]any{"field": field},
	})
}

func (q *BoolQuery) AddTerms(field string, values []string) {
	q.AddFilter(map[string]any{
		"terms": map[string]any{field: values},
	})
}

func (q *BoolQuery) MustCount() int { return len(q.Must) }

// Document field names, copied verbatim from
// PhabricatorSearchDocumentFieldType. They are the literal keys a document is
// written under, so they must match the PHP constants character for character.
const (
	FieldTitle   = "titl"
	FieldBody    = "body"
	FieldComment = "cmnt"
	FieldAll     = "full"
	FieldCore    = "core"
)

// Relationship names, copied verbatim from PhabricatorSearchRelationship. The
// last three are status markers rather than links to another object: a
// document carries "open" or "clos" to say which it is, and "unow" to say it
// has no owner, so a query for open documents is an exists check rather than a
// term match.
const (
	RelAuthor     = "auth"
	RelBook       = "book"
	RelReviewer   = "revw"
	RelSubscriber = "subs"
	RelCommenter  = "comm"
	RelOwner      = "ownr"
	RelProject    = "proj"
	RelRepository = "repo"

	RelOpen    = "open"
	RelClosed  = "clos"
	RelUnowned = "unow"
)

// SubfieldCJK is the analysed subfield that makes CJK text searchable. It sits
// beside raw, keywords and stems, whose analyser chains are all English: the
// letter tokenizer treats a run of Han characters as one indivisible token and
// the standard tokenizer splits it into single characters, so neither can
// answer a two-character Chinese query with any precision.
//
// Removing it does not break anything loudly. Indexing keeps working, queries
// keep answering 200, and CJK results quietly degrade to whatever the English
// chains happen to match. See compat/phorge/README.md section 七.
const SubfieldCJK = "cjk"

// AllFields returns every indexable document field name.
func AllFields() []string {
	return []string{FieldTitle, FieldBody, FieldComment, FieldAll, FieldCore}
}

// AllRelationships returns every relationship name.
func AllRelationships() []string {
	return []string{
		RelAuthor, RelBook, RelReviewer, RelSubscriber,
		RelCommenter, RelOwner, RelProject, RelRepository,
		RelOpen, RelClosed, RelUnowned,
	}
}
