package integrations

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func TestInboundNormalization(t *testing.T) {
	fixtures := []Intake{
		{Provider: "mailgun", Fields: map[string]string{"message-headers": `[["Message-ID","<a>"]]`, "recipient": "to@test", "from": "from@test", "stripped-html": "<b>a</b>"}},
		{Provider: "sendgrid", Fields: map[string]string{"headers": "Message-ID: <a>\n", "to": "to@test", "from": "from@test", "html": "<b>a</b>"}},
		{Provider: "postmark", Postmark: json.RawMessage(`{"To":"to@test","From":"from@test","MessageID":"<a>","HtmlBody":"<b>a</b>","Attachments":[{"Name":"a.txt","Content":"YQ=="}]}`)},
	}
	for _, in := range fixtures {
		m, e := Normalize(in)
		if e != nil || m.Headers["message-id"] != "<a>" || m.HTML != "<b>a</b>" {
			t.Fatalf("%s: %#v %v", in.Provider, m, e)
		}
	}
	bad := fixtures[0]
	bad.Attachments = []Attachment{{Name: "bad", Data: "not-base64"}}
	if _, e := Normalize(bad); e == nil {
		t.Fatal("accepted corrupt attachment")
	}
	bad = fixtures[0]
	bad.Fields = map[string]string{"message-headers": "broken"}
	if _, e := Normalize(bad); e == nil {
		t.Fatal("accepted malformed headers")
	}
}
func TestCredentialRotationDoesNotChangeIdentity(t *testing.T) {
	r := Request{ID: "id", Target: "asana", Principal: "account", Method: "POST", Path: "tasks", Secret: "first"}
	target := Target{Type: "asana", URL: "https://app.asana.com/api/1.0"}
	a, _ := r.Hash(target)
	r.Secret = "rotated"
	b, _ := r.Hash(target)
	if a != b {
		t.Fatal("OAuth token stored in identity")
	}
	r.Principal = "other"
	b, _ = r.Hash(target)
	if a == b {
		t.Fatal("actors not isolated")
	}
}
func TestOperationAndConfigBoundaries(t *testing.T) {
	target := Target{Type: "asana"}
	for _, path := range []string{"../tasks", "tasks/1?token=x", "tasks/1/../../users", "https://evil.test/tasks"} {
		r := Request{ID: "id", Path: path, Method: "POST", Secret: "secret"}
		if r.Validate(target) == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	c := Config{Token: "t", DSN: "dsn", Targets: map[string]Target{"asana": {Type: "asana", URL: "https://evil.test/api/1.0"}}}
	if c.Validate() == nil {
		t.Fatal("allowed OAuth credential exfiltration endpoint")
	}
	c.Targets = map[string]Target{"twilio": {Type: "twilio", URL: "https://api.twilio.com/2010-04-01", User: "ACtest", Secret: "secret", From: "+1234567890"}}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
}
func TestOAuthRSASignature(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	target := Target{ConsumerKey: "consumer", PrivateKey: string(private)}
	uri := "https://jira.test/jira/rest/api/2/issue/T-1?expand=names"
	header, e := oauthHeader(target, "access", "GET", uri)
	if e != nil {
		t.Fatal(e)
	}
	params := map[string]string{}
	for _, field := range strings.Split(strings.TrimPrefix(header, "OAuth "), ", ") {
		k, v, _ := strings.Cut(field, "=")
		v, e := url.QueryUnescape(strings.Trim(v, `"`))
		if e != nil {
			t.Fatal(e)
		}
		params[k] = v
	}
	signature := params["oauth_signature"]
	delete(params, "oauth_signature")
	pairs := []string{}
	for k, v := range params {
		pairs = append(pairs, escape(k)+"="+escape(v))
	}
	pairs = append(pairs, "expand=names")
	sort.Strings(pairs)
	base := "GET&" + escape("https://jira.test/jira/rest/api/2/issue/T-1") + "&" + escape(strings.Join(pairs, "&"))
	hash := sha1.Sum([]byte(base))
	sig, e := base64.StdEncoding.DecodeString(signature)
	if e != nil {
		t.Fatal(e)
	}
	if e = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA1, hash[:], sig); e != nil {
		t.Fatal(e)
	}
}
func TestTransportDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	s := New(Config{}, Store{})
	_, status, e := Send(context.Background(), s.HTTP, Target{Type: "asana", URL: origin.URL}, Request{Path: "tasks", Method: "POST", Secret: "private"})
	if e != nil || status != 307 || leaked {
		t.Fatalf("redirect credentials leak: %d %v %v", status, e, leaked)
	}
}
func TestEffectReplayNeverSendsTwice(t *testing.T) {
	db, m, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	m.ExpectBegin()
	m.ExpectExec("INSERT INTO gorge_integration_effect").WithArgs("id", "hash", "twilio", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT digest,state,result,httpStatus").WithArgs("id").WillReturnRows(sqlmock.NewRows([]string{"digest", "state", "result", "httpStatus"}).AddRow("hash", "accepted", []byte(`{"sid":"sent"}`), 201))
	m.ExpectRollback()
	called := false
	o, e := (Store{db}).Effect(context.Background(), "id", "hash", "twilio", func() (json.RawMessage, int, error) { called = true; return nil, 0, nil })
	if e != nil || o.State != "accepted" || called {
		t.Fatalf("replay sent: %#v %v", o, e)
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestEffectUnknownStopsSubmission(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m.ExpectBegin()
	m.ExpectExec("INSERT INTO gorge_integration_effect").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT digest,state,result,httpStatus").WillReturnRows(sqlmock.NewRows([]string{"digest", "state", "result", "httpStatus"}).AddRow("hash", "submitting", nil, 0))
	m.ExpectRollback()
	_, e := (Store{db}).Effect(context.Background(), "id", "hash", "sns", func() (json.RawMessage, int, error) { t.Fatal("repeated uncertain SMS"); return nil, 0, nil })
	if !errors.Is(e, ErrUnknown) {
		t.Fatal(e)
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestFactEmptySnapshotClearsOldData(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m.ExpectBegin()
	m.ExpectExec("INSERT INTO fact_objectdimension").WithArgs("PHID-TASK-one").WillReturnResult(sqlmock.NewResult(7, 1))
	m.ExpectExec("DELETE FROM fact_intdatapoint").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 3))
	m.ExpectCommit()
	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	e = applyFacts(context.Background(), tx, FactPage{Objects: []FactObject{{PHID: "PHID-TASK-one"}}})
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestFactRejectsCrossObjectAndOverflow(t *testing.T) {
	p := FactPage{Objects: []FactObject{{PHID: "one", Facts: []Fact{{Key: "a", Object: "other", Value: "1"}}}}}
	if p.Validate() == nil {
		t.Fatal("accepted cross-object facts")
	}
	p.Objects[0].Facts[0].Object = "one"
	p.Objects[0].Facts[0].Value = "9223372036854775808"
	if p.Validate() == nil {
		t.Fatal("accepted int64 overflow")
	}
}

func TestFactCursorMonotonic(t *testing.T) {
	for _, v := range [][3]string{{"", "1", "true"}, {"15:1", "15:2", "true"}, {"15:2", "16:1", "true"}, {"15:2", "15:1", "false"}, {"15:2", "15:2", "false"}, {"15:2", "14:9", "false"}, {"1", "1:1", "false"}, {"", "broken", "false"}} {
		if factPositionAfter(v[0], v[1]) != (v[2] == "true") {
			t.Fatalf("cursor %v", v)
		}
	}
}
func TestTwilioAndSNSTransports(t *testing.T) {
	for _, kind := range []string{"twilio", "sns"} {
		t.Run(kind, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Error("incorrect submission method")
				}
				if e := r.ParseForm(); e != nil {
					t.Error(e)
				}
				if kind == "twilio" {
					user, secret, ok := r.BasicAuth()
					if !ok || user != "account" || secret != "secret" || r.Form.Get("To") != "+15550000001" || r.Form.Get("Body") != "hello" {
						t.Error("SMS credentials or message changed")
					}
					_, _ = w.Write([]byte(`{"sid":"accepted"}`))
				} else {
					if !strings.Contains(r.Header.Get("Authorization"), "/sns/aws4_request") || r.Form.Get("PhoneNumber") != "+15550000001" || r.Form.Get("Action") != "Publish" {
						t.Error("SNS signing or fields changed")
					}
					_, _ = w.Write([]byte(`<PublishResponse><PublishResult><MessageId>accepted</MessageId></PublishResult></PublishResponse>`))
				}
			}))
			defer origin.Close()
			s := New(Config{}, Store{})
			raw, status, e := Send(context.Background(), s.HTTP, Target{Type: kind, URL: origin.URL, User: "account", Secret: "secret", From: "+15550000002", Region: "us-east-1"}, Request{To: "+15550000001", Text: "hello"})
			if e != nil || status != 200 || !strings.Contains(string(raw), "accepted") {
				t.Fatalf("SMS result %s %d %v", raw, status, e)
			}
		})
	}
}

func TestAsanaWorkerOperationCoverage(t *testing.T) {
	target := Target{Type: "asana"}
	for _, op := range [][2]string{{"POST", "tasks"}, {"PUT", "tasks/1"}, {"DELETE", "tasks/1"}, {"POST", "tasks/1/removeFollowers"}, {"POST", "tasks/1/addFollowers"}, {"POST", "tasks/1/addProject"}, {"POST", "tasks/1/stories"}} {
		r := Request{ID: "id", Principal: "account", Secret: "token", Method: op[0], Path: op[1]}
		if e := r.Validate(target); e != nil {
			t.Fatalf("missing worker operation %v: %v", op, e)
		}
	}
	r := Request{ID: "id", Principal: "account", Secret: "token", Method: "DELETE", Path: "workspaces/1"}
	if r.Validate(target) == nil {
		t.Fatal("workspace deletion permitted")
	}
}

func TestGitHubReadBoundaries(t *testing.T) {
	target := Target{Type: "github"}
	r := Request{ID: "read", Principal: "context", Secret: "oauth", Method: "GET", Path: "repos/owner/repo/issues/1"}
	if e := r.Validate(target); e != nil {
		t.Fatal(e)
	}
	r.Method = "POST"
	if r.Validate(target) == nil {
		t.Fatal("GitHub mutation allowed")
	}
	r.Method = "GET"
	r.Path = "repos/../repo/issues/1"
	if r.Validate(target) == nil {
		t.Fatal("GitHub traversal allowed")
	}
}

func TestIdenticalMailWithoutMessageIDIsNotMerged(t *testing.T) {
	in := Intake{Provider: "sendgrid", Fields: map[string]string{"to": "to@test", "from": "from@test", "text": "same"}, IngressID: strings.Repeat("a", 32)}
	a, e := Normalize(in)
	if e != nil {
		t.Fatal(e)
	}
	in.IngressID = strings.Repeat("b", 32)
	b, e := Normalize(in)
	if e != nil {
		t.Fatal(e)
	}
	ha, _, _ := digest(a)
	hb, _, _ := digest(b)
	if ha == hb {
		t.Fatal("two valid incoming messages merged")
	}
	in.IngressID = ""
	if _, e = Normalize(in); e == nil {
		t.Fatal("missing ingress identity accepted")
	}
}
