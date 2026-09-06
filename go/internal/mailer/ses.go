package mailer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type sesAdapter struct {
	accessKey string
	secretKey string
	region    string
	endpoint  string
}

func newSESAdapter(opts map[string]string) (*sesAdapter, error) {
	a := &sesAdapter{
		accessKey: opts["access-key"],
		secretKey: opts["secret-key"],
		region:    opts["region"],
		endpoint:  opts["endpoint"],
	}
	if a.region == "" {
		a.region = "us-east-1"
	}
	if a.endpoint == "" {
		a.endpoint = fmt.Sprintf("https://email.%s.amazonaws.com", a.region)
	}
	return a, nil
}

func (a *sesAdapter) Type() string { return "ses" }

// Send posts the message through SendRawEmail, so the MIME this service builds
// is what SES relays — attachments, custom headers and all. The higher-level
// SendEmail action would drop everything the contract carries beyond a body.
func (a *sesAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	raw := buildMIME(msg)

	params := url.Values{}
	params.Set("Action", "SendRawEmail")
	params.Set("RawMessage.Data", base64.StdEncoding.EncodeToString(raw))
	params.Set("Source", formatAddr(msg.From))
	for i, addr := range recipients(msg) {
		params.Set(fmt.Sprintf("Destinations.member.%d", i+1), addr)
	}

	body := params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ses: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	a.signV4(req, []byte(body), time.Now().UTC())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ses: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if err := classifyProviderStatus("ses", resp.StatusCode, respBody); err != nil {
		return "", err
	}

	var result struct {
		SendRawEmailResult struct {
			MessageId string `xml:"MessageId"`
		}
	}
	if err := xml.Unmarshal(respBody, &result); err == nil {
		return result.SendRawEmailResult.MessageId, nil
	}
	// The message went out; only the id is unreadable, and it is optional.
	return "", nil
}

// signV4 signs the request with AWS Signature Version 4. The request is always
// a POST to "/" with the same three signed headers, so the canonical request is
// assembled directly rather than derived from req.
func (a *sesAdapter) signV4(req *http.Request, payload []byte, now time.Time) {
	service := "ses"
	datestamp := now.Format("20060102")
	amzdate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzdate)
	req.Header.Set("Host", req.URL.Host)

	signedHeaders := "content-type;host;x-amz-date"
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-date:%s\n",
		req.Header.Get("Content-Type"), req.URL.Host, amzdate)

	canonicalRequest := strings.Join([]string{
		"POST",
		"/",
		"",
		canonicalHeaders,
		signedHeaders,
		sha256Hex(payload),
	}, "\n")

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, a.region, service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzdate, scope, sha256Hex([]byte(canonicalRequest)))

	signingKey := deriveKey(a.secretKey, datestamp, a.region, service)
	signature := hmacSHA256Hex(signingKey, []byte(stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		a.accessKey, scope, signedHeaders, signature))
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func hmacSHA256Hex(key, data []byte) string {
	return fmt.Sprintf("%x", hmacSHA256(key, data))
}

func deriveKey(secret, datestamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}
