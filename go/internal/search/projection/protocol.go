// Package projection defines the versioned delivery boundary. It deliberately
// does not allocate revisions or turn missing export results into deletions.
package projection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/soulteary/gorge/go/internal/contracts"
)

var phidPattern = regexp.MustCompile(`^PHID-[A-Z0-9]{4}-[a-zA-Z0-9]+$`)
var namespacePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

const MaxDocumentBytes = 2 * 1024 * 1024

// DocumentHash sorts object keys but preserves array ordering and integer
// spelling. Document has no floats/maps: this restricted canonical format is
// shared with PhabricatorSearchDocumentSerializer, not general-purpose JCS.
func DocumentHash(doc *contracts.Document) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("document required")
	}
	values := []string{doc.PHID, doc.Type, doc.Title}
	for _, f := range doc.Fields {
		values = append(values, f.Name, f.Corpus, f.Aux)
	}
	for _, r := range doc.Relationships {
		values = append(values, r.Name, r.RelatedPHID, r.RType)
	}
	for _, value := range values {
		if !utf8.ValidString(value) {
			return "", fmt.Errorf("invalid UTF-8 document")
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var tree any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&tree); err != nil {
		return "", err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(tree); err != nil {
		return "", err
	}
	if out.Len() > MaxDocumentBytes+1 {
		return "", fmt.Errorf("document exceeds 2 MiB")
	}
	digest := sha256.Sum256(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
	return hex.EncodeToString(digest[:]), nil
}

// PayloadHash covers tombstone identity as well as upsert content.
func PayloadHash(event *contracts.SearchProjection) (string, error) {
	if event == nil {
		return "", fmt.Errorf("projection required")
	}
	if event.Operation == "upsert" {
		return DocumentHash(event.Document)
	}
	if event.Operation != "delete" {
		return "", fmt.Errorf("invalid operation")
	}
	raw, _ := json.Marshal(map[string]string{"operation": "delete", "phid": event.PHID, "type": event.Type})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func Validate(event *contracts.SearchProjection) (int64, error) {
	if event == nil || event.ProjectionVersion != 1 {
		return 0, fmt.Errorf("projectionVersion must be 1")
	}
	if !utf8.ValidString(event.EventID) || event.EventID == "" || len(event.EventID) > 128 || strings.ContainsAny(event.EventID, "\x00\r\n") {
		return 0, fmt.Errorf("invalid eventID")
	}
	if !namespacePattern.MatchString(event.Namespace) {
		return 0, fmt.Errorf("invalid namespace")
	}
	if len(event.PHID) > 64 || !phidPattern.MatchString(event.PHID) || event.Type != strings.Split(event.PHID, "-")[1] {
		return 0, fmt.Errorf("invalid PHID/type")
	}
	revision, err := strconv.ParseInt(event.Revision, 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != event.Revision {
		return 0, fmt.Errorf("revision must be a positive canonical int64 string")
	}
	if !utf8.ValidString(event.SerializerVersion) || !utf8.ValidString(event.SourceVersion) || event.SerializerVersion == "" || len(event.SerializerVersion) > 128 || event.SourceVersion == "" || len(event.SourceVersion) > 512 {
		return 0, fmt.Errorf("serializerVersion and sourceVersion required")
	}
	switch event.Operation {
	case "upsert":
		if event.Document == nil || event.Document.PHID != event.PHID || event.Document.Type != event.Type {
			return 0, fmt.Errorf("document identity mismatch")
		}
	case "delete":
		if event.Document != nil {
			return 0, fmt.Errorf("delete must not include a document")
		}
	default:
		return 0, fmt.Errorf("invalid operation")
	}
	digest, err := PayloadHash(event)
	if err != nil {
		return 0, err
	}
	if digest != event.PayloadHash {
		return 0, fmt.Errorf("payloadHash mismatch")
	}
	return revision, nil
}

// Decode rejects unknown keys and trailing JSON rather than silently dropping
// parts of a projection during protocol negotiation.
func Decode(raw []byte) (*contracts.SearchProjection, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid UTF-8 projection")
	}
	if len(raw) > MaxDocumentBytes+4096 {
		return nil, fmt.Errorf("projection too large")
	}
	keyDecoder := json.NewDecoder(bytes.NewReader(raw))
	keyDecoder.UseNumber()
	if err := uniqueJSONKeys(keyDecoder, 0); err != nil {
		return nil, err
	}
	var event contracts.SearchProjection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing projection JSON")
	}
	if _, err := Validate(&event); err != nil {
		return nil, err
	}
	return &event, nil
}
