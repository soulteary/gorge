package integrations

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	ID        string         `json:"id"`
	Target    string         `json:"target"`
	Principal string         `json:"principal,omitempty"`
	Method    string         `json:"method,omitempty"`
	Path      string         `json:"path,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	To        string         `json:"to,omitempty"`
	Text      string         `json:"text,omitempty"`
	// OAuth access tokens are never stored, returned, included in identity hashes or logged.
	Secret string `json:"secret,omitempty"`
	ETag   string `json:"etag,omitempty"`
}

var phone = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
var asanaPath = regexp.MustCompile(`^(tasks(/[0-9]+)?(/(addFollowers|removeFollowers|addProject|stories))?|workspaces(/[0-9]+)?|users/me)$`)
var githubPath = regexp.MustCompile(`^(user|users|repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/(events|issues/events|issues/[1-9][0-9]*))$`)
var jiraPath = regexp.MustCompile(`^(rest/api/2/issue/[A-Za-z0-9_-]+(/(comment|remotelink))?|rest/api/3/myself|rest/auth/1/session|rest/api/2/user)$`)

func (r Request) Validate(t Target) error {
	if r.ID == "" || len(r.ID) > 128 || len(r.Principal) > 128 || len(r.Secret) > 8192 || strings.ContainsAny(r.Secret, "\r\n") || len(r.ETag) > 1024 || strings.ContainsAny(r.ETag, "\r\n") {
		return errors.New("invalid effect identity")
	}
	switch t.Type {
	case "twilio", "sns":
		if !phone.MatchString(r.To) || r.Text == "" || len(r.Text) > 4096 {
			return errors.New("invalid SMS")
		}
	case "asana":
		if !asanaPath.MatchString(r.Path) || r.Secret == "" || r.Principal == "" || (r.Method != "GET" && r.Method != "POST" && r.Method != "PUT" && r.Method != "DELETE") {
			return errors.New("invalid Asana operation")
		}
		task := regexp.MustCompile(`^tasks/[0-9]+$`).MatchString(r.Path)
		if (r.Method == "DELETE" || r.Method == "PUT") && !task {
			return errors.New("invalid Asana task mutation")
		}
		if r.Method != "GET" && (strings.HasPrefix(r.Path, "workspaces") || r.Path == "users/me") {
			return errors.New("read-only Asana identity/workspace")
		}
		if strings.Contains(r.Path, "/add") || strings.HasSuffix(r.Path, "/removeFollowers") || strings.HasSuffix(r.Path, "/stories") {
			if r.Method != "POST" {
				return errors.New("invalid Asana task action")
			}
		}
	case "github":
		if !githubPath.MatchString(r.Path) || r.Method != "GET" || r.Secret == "" || r.Principal == "" {
			return errors.New("invalid GitHub operation")
		}
		for _, segment := range strings.Split(r.Path, "/") {
			if segment == "." || segment == ".." {
				return errors.New("invalid GitHub path")
			}
		}
	case "jira":
		if r.Method != "GET" && !strings.HasSuffix(r.Path, "/comment") && !strings.HasSuffix(r.Path, "/remotelink") {
			return errors.New("invalid JIRA write")
		}
		if !jiraPath.MatchString(r.Path) || r.Secret == "" || r.Principal == "" || (r.Method != "POST" && r.Method != "GET") {
			return errors.New("invalid JIRA operation")
		}
	default:
		return errors.New("unsupported target")
	}
	return nil
}
func (r Request) Hash(t Target) (string, error) {
	r.Secret = ""
	h, _, e := digest(struct {
		Request   Request
		Type, URL string
	}{r, t.Type, t.URL})
	return h, e
}
func form(params map[string]any) (url.Values, error) {
	v := url.Values{}
	for k, x := range params {
		switch a := x.(type) {
		case string:
			v.Set(k, a)
		case bool:
			if a {
				v.Set(k, "true")
			} else {
				v.Set(k, "false")
			}
		case float64:
			v.Set(k, strconv.FormatFloat(a, 'f', -1, 64))
		case nil:
		case []any:
			for index, item := range a {
				b, e := json.Marshal(item)
				if e != nil {
					return nil, e
				}
				if s, ok := item.(string); ok {
					v.Add(fmt.Sprintf("%s[%d]", k, index), s)
				} else {
					v.Add(fmt.Sprintf("%s[%d]", k, index), string(b))
				}
			}
		default:
			b, e := json.Marshal(x)
			if e != nil {
				return nil, e
			}
			v.Set(k, string(b))
		}
	}
	return v, nil
}
func mustJSON(v any) []byte  { b, _ := json.Marshal(v); return b }
func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func oauthHeader(t Target, token, method, uri string, extra ...url.Values) (string, error) {
	block, _ := pem.Decode([]byte(t.PrivateKey))
	if block == nil {
		return "", errors.New("invalid OAuth signing key")
	}
	key, e := x509.ParsePKCS1PrivateKey(block.Bytes)
	if e != nil {
		v, e2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e2 != nil {
			return "", errors.New("invalid OAuth signing key")
		}
		var ok bool
		key, ok = v.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("OAuth key is not RSA")
		}
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	p := map[string]string{"oauth_consumer_key": t.ConsumerKey, "oauth_token": token, "oauth_signature_method": "RSA-SHA1", "oauth_timestamp": "", "oauth_nonce": hex.EncodeToString(nonce), "oauth_version": "1.0"}
	p["oauth_timestamp"] = strconv.FormatInt(time.Now().Unix(), 10)
	if token == "" {
		delete(p, "oauth_token")
	}
	bodyParams := url.Values{}
	for _, values := range extra {
		for k, vs := range values {
			for _, value := range vs {
				if strings.HasPrefix(k, "oauth_") && k != "oauth_verifier" {
					p[k] = value
				} else {
					bodyParams.Add(k, value)
				}
			}
		}
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		pairs = append(pairs, escape(k)+"="+escape(p[k]))
	}
	parsed, e := url.Parse(uri)
	if e != nil {
		return "", e
	}
	for k, values := range parsed.Query() {
		for _, v := range values {
			pairs = append(pairs, escape(k)+"="+escape(v))
		}
	}
	for k, vs := range bodyParams {
		for _, value := range vs {
			pairs = append(pairs, escape(k)+"="+escape(value))
		}
	}
	sort.Strings(pairs)
	parsed.RawQuery = ""
	base := method + "&" + escape(parsed.String()) + "&" + escape(strings.Join(pairs, "&"))
	h := sha1.Sum([]byte(base))
	sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, h[:])
	if e != nil {
		return "", e
	}
	p["oauth_signature"] = base64.StdEncoding.EncodeToString(sig)
	keys = append(keys, "oauth_signature")
	sort.Strings(keys)
	pairs = nil
	for _, k := range keys {
		pairs = append(pairs, escape(k)+`="`+escape(p[k])+`"`)
	}
	return "OAuth " + strings.Join(pairs, ", "), nil
}
func Send(ctx context.Context, client *http.Client, t Target, r Request) (json.RawMessage, int, error) {
	return send(ctx, client, t, r, nil)
}

func SendRead(ctx context.Context, client *http.Client, t Target, r Request) (Outcome, error) {
	headers := map[string]string{}
	raw, status, err := send(ctx, client, t, r, headers)
	return Outcome{State: "read", Result: raw, Status: status, Headers: headers}, err
}

func send(ctx context.Context, client *http.Client, t Target, r Request, headers map[string]string) (json.RawMessage, int, error) {
	var raw []byte
	var uri, method, ctype string
	method = r.Method
	uri = strings.TrimRight(t.URL, "/") + "/" + r.Path
	ctype = "application/json"
	switch t.Type {
	case "twilio":
		uri = strings.TrimRight(t.URL, "/") + "/Accounts/" + url.PathEscape(t.User) + "/Messages.json"
		method = "POST"
		raw = []byte(url.Values{"From": {t.From}, "To": {r.To}, "Body": {r.Text}}.Encode())
		ctype = "application/x-www-form-urlencoded"
	case "sns":
		uri = t.URL
		method = "POST"
		raw = []byte(url.Values{"Action": {"Publish"}, "Version": {"2010-03-31"}, "PhoneNumber": {r.To}, "Message": {r.Text}}.Encode())
		ctype = "application/x-www-form-urlencoded"
	case "asana", "github":
		v, e := form(r.Params)
		if e != nil {
			return nil, 0, e
		}
		if method == "GET" {
			if len(v) > 0 {
				uri += "?" + v.Encode()
			}
		} else {
			raw = []byte(v.Encode())
			ctype = "application/x-www-form-urlencoded"
		}
	case "jira":
		if method == "GET" {
			v, e := form(r.Params)
			if e != nil {
				return nil, 0, e
			}
			if len(v) > 0 {
				uri += "?" + v.Encode()
			}
		}
		var e error
		if method != "GET" {
			raw, e = json.Marshal(r.Params)
		}
		if e != nil {
			return nil, 0, e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, uri, bytes.NewReader(raw))
	if e != nil {
		return nil, 0, errors.New("invalid provider request")
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Accept", "application/json")
	switch t.Type {
	case "twilio":
		req.SetBasicAuth(t.User, t.Secret)
	case "asana", "github":
		req.Header.Set("Authorization", "Bearer "+r.Secret)
		req.Header.Set("User-Agent", "Gorge-integrations")
	case "jira":
		h, e := oauthHeader(t, r.Secret, method, uri)
		if e != nil {
			return nil, 0, e
		}
		req.Header.Set("Authorization", h)
	case "sns":
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		e := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: t.User, SecretAccessKey: t.Secret}, req, hash, "sns", t.Region, time.Now())
		if e != nil {
			return nil, 0, errors.New("SNS signing failed")
		}
	}
	if t.Type == "github" && r.Method == "GET" && r.ETag != "" {
		req.Header.Set("If-None-Match", r.ETag)
	}
	resp, e := client.Do(req)
	if e != nil {
		return nil, 0, errors.New("provider outcome unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if headers != nil {
		for _, k := range []string{"ETag", "X-Poll-Interval", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"} {
			if v := resp.Header.Get(k); v != "" && len(v) <= 1024 {
				headers[strings.ToLower(k)] = v
			}
		}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if e != nil || len(b) > 1024*1024 {
		return nil, resp.StatusCode, errors.New("provider response unavailable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return json.RawMessage(`{}`), resp.StatusCode, nil
	}
	if t.Type == "sns" {
		var out struct {
			Result struct {
				MessageID string `xml:"MessageId"`
			} `xml:"PublishResult"`
		}
		if xml.Unmarshal(b, &out) != nil || out.Result.MessageID == "" {
			return nil, resp.StatusCode, errors.New("invalid SNS response")
		}
		return mustJSON(map[string]string{"messageID": out.Result.MessageID}), resp.StatusCode, nil
	}
	if len(b) == 0 {
		return json.RawMessage(`{}`), resp.StatusCode, nil
	}
	if !json.Valid(b) {
		return nil, resp.StatusCode, errors.New("invalid provider response")
	}
	if t.Type == "asana" {
		var out struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &out) != nil || len(out.Data) == 0 {
			return nil, resp.StatusCode, errors.New("invalid Asana response")
		}
		return out.Data, resp.StatusCode, nil
	}
	return b, resp.StatusCode, nil
}
