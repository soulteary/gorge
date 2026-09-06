package contracttest

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The path walker is the one piece of this package the domain fixture tests
// cannot pin on their own: a fixture that fails to resolve a path and a fixture
// whose assertion is genuinely wrong look identical in the failure output. The
// dotted-key cases below are what the notification admin port's GET /status/
// response forced — it answers a flat map whose keys contain literal dots, such
// as "clients.active" and "messages.in", because Phorge's cluster notification
// panel reads them that way.
func TestLookupJSONPath(t *testing.T) {
	const document = `{
		"data": {
			"html": "<span>",
			"parts": [{"type": "="}, {"type": "-"}],
			"empty": null
		},
		"instance": "default",
		"clients.active": 2,
		"messages.in": 0,
		"history.age": null,
		"history": {"size": 1}
	}`

	var decoded map[string]any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want string // fmt-formatted value, "" when the lookup must fail
		ok   bool
	}{
		// Nested paths, which is all the render and diff fixtures use.
		{path: "data.html", want: "<span>", ok: true},
		{path: "data.parts.0.type", want: "=", ok: true},
		{path: "data.parts.1.type", want: "-", ok: true},
		{path: "instance", want: "default", ok: true},

		// Literal dotted keys.
		{path: "clients.active", want: "2", ok: true},
		{path: "messages.in", want: "0", ok: true},

		// A literal key present but null resolves, and reports null. jsonHas
		// rejects that on its own; the walker must not conflate it with an
		// absent key or "history.age" could never be asserted absent.
		{path: "history.age", want: "<nil>", ok: true},
		{path: "data.empty", want: "<nil>", ok: true},

		// "history" exists as a nested object too, so this pair is what the
		// backtracking is for: the literal "history.age" wins over splitting,
		// while "history.size" still resolves by splitting.
		{path: "history.size", want: "1", ok: true},

		// Absent, out of range, and walking into a scalar.
		{path: "clients.total", ok: false},
		{path: "data.parts.2", ok: false},
		{path: "data.parts.x", ok: false},
		{path: "instance.name", ok: false},
		{path: "clients.active.more", ok: false},
	}

	for _, tc := range cases {
		got, ok := lookupJSONPath(decoded, tc.path)
		if ok != tc.ok {
			t.Errorf("lookupJSONPath(%q) resolved = %v, want %v", tc.path, ok, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		// fmt.Sprint is how the assertions themselves compare values, so
		// comparing that way keeps this test honest about what a fixture sees.
		if formatted := fmt.Sprint(got); formatted != tc.want {
			t.Errorf("lookupJSONPath(%q) = %q, want %q", tc.path, formatted, tc.want)
		}
	}
}

func TestAssertsStructure(t *testing.T) {
	var fx Fixture
	if fx.assertsStructure() {
		t.Error("a fixture with no expectations must not require a JSON body")
	}

	// The notification client port's 501 answers plain text, so this is the
	// shape that must stay exempt from decoding.
	fx.Expect.Status = 501
	fx.Expect.BodyContains = []string{"HTTP/501 Use Websockets\n"}
	if fx.assertsStructure() {
		t.Error("status and bodyContains alone must not require a JSON body")
	}

	fx.Expect.JSONEquals = map[string]any{"fingerprint": "x"}
	if !fx.assertsStructure() {
		t.Error("a jsonEquals assertion requires a JSON body")
	}
}
