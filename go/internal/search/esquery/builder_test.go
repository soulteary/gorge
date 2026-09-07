package esquery

import (
	"encoding/json"
	"strings"
	"testing"
)

// The four-character names are Phorge's, not this service's. They reach the
// index as literal field names, so a document written under one spelling is
// unfindable by a query written under another — and nothing anywhere reports
// an error, the results are simply empty.
//
// The table below is a transcription of PhabricatorSearchDocumentFieldType and
// PhabricatorSearchRelationship. Changing a value here without changing the
// PHP constant makes every existing index unsearchable. See
// compat/phorge/README.md section 七.
func TestNamesMatchThePHPConstants(t *testing.T) {
	cases := []struct {
		phpConstant string
		got         string
		want        string
	}{
		{"PhabricatorSearchDocumentFieldType::FIELD_TITLE", FieldTitle, "titl"},
		{"PhabricatorSearchDocumentFieldType::FIELD_BODY", FieldBody, "body"},
		{"PhabricatorSearchDocumentFieldType::FIELD_COMMENT", FieldComment, "cmnt"},
		{"PhabricatorSearchDocumentFieldType::FIELD_ALL", FieldAll, "full"},
		{"PhabricatorSearchDocumentFieldType::FIELD_CORE", FieldCore, "core"},

		{"PhabricatorSearchRelationship::RELATIONSHIP_AUTHOR", RelAuthor, "auth"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_BOOK", RelBook, "book"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_REVIEWER", RelReviewer, "revw"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_SUBSCRIBER", RelSubscriber, "subs"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_COMMENTER", RelCommenter, "comm"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_OWNER", RelOwner, "ownr"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_PROJECT", RelProject, "proj"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_REPOSITORY", RelRepository, "repo"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_OPEN", RelOpen, "open"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_CLOSED", RelClosed, "clos"},
		{"PhabricatorSearchRelationship::RELATIONSHIP_UNOWNED", RelUnowned, "unow"},
	}

	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s is %q, this package says %q", tc.phpConstant, tc.want, tc.got)
		}
	}
}

// Every constant has to appear in the list the index mapping is built from. A
// field missing here is never given a mapping, so documents written to it are
// indexed under whatever the cluster's dynamic mapping guesses — which is not
// an error either.
func TestTheListsCoverEveryConstant(t *testing.T) {
	if got, want := len(AllFields()), 5; got != want {
		t.Errorf("expected %d fields, got %d: %v", want, got, AllFields())
	}
	if got, want := len(AllRelationships()), 11; got != want {
		t.Errorf("expected %d relationships, got %d: %v", want, got, AllRelationships())
	}

	seen := make(map[string]bool)
	for _, name := range append(AllFields(), AllRelationships()...) {
		if len(name) != 4 {
			t.Errorf("%q is not four characters: Phorge's names all are", name)
		}
		if seen[name] {
			t.Errorf("%q appears twice", name)
		}
		seen[name] = true
	}
}

// An untouched BoolQuery has to serialise to an empty object rather than to
// four empty arrays: an explicit empty must list is not the same query.
func TestBoolQueryOmitsEmptyClauseLists(t *testing.T) {
	raw, err := json.Marshal(&BoolQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("expected an empty object, got %s", raw)
	}
}

func TestBoolQueryAccumulatesClauses(t *testing.T) {
	q := &BoolQuery{}
	q.AddMust("m")
	q.AddShould("s")
	q.AddMustNot("n")
	q.AddExists(RelOpen)
	q.AddTerms(RelAuthor, []string{"PHID-USER-1"})

	if q.MustCount() != 1 || len(q.Should) != 1 || len(q.MustNot) != 1 {
		t.Fatalf("unexpected clause counts: %+v", q)
	}
	// Both exists and terms are filters rather than musts: they select
	// documents without contributing to the relevance score.
	if len(q.Filter) != 2 {
		t.Fatalf("expected exists and terms in filter, got %d", len(q.Filter))
	}

	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"must"`, `"should"`, `"must_not"`, `"filter"`, `"open"`, `"auth"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("expected %s in %s", want, raw)
		}
	}
}
