package filestorage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// s3KeyPattern is the key layout the PHP engine uses, and the reason this
// engine mints keys itself rather than letting the SDK choose. Changing it
// leaves every existing object in place and unreachable, silently. See
// compat/phorge/README.md section 7.
var s3KeyPattern = regexp.MustCompile(`^phabricator(/[^/]+)?/[a-f0-9]{2}/[a-f0-9]{2}/[a-f0-9]{16}$`)

func TestS3KeyLayout(t *testing.T) {
	plain, err := NewS3Engine(S3Config{Bucket: "files", Region: "us-east-1", Endpoint: "https://s3.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	key, err := plain.generateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !s3KeyPattern.MatchString(key) {
		t.Errorf("key %q does not match the Phorge layout", key)
	}
	if !strings.HasPrefix(key, "phabricator/") {
		t.Errorf("the phabricator prefix is part of the contract, got %q", key)
	}

	// The instance name is what lets several Phorge instances share a bucket,
	// so it has to appear as its own path segment.
	named, err := NewS3Engine(S3Config{Bucket: "files", InstanceName: "inst", Endpoint: "https://s3.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	key, err = named.generateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "phabricator/inst/") {
		t.Errorf("expected the instance segment, got %q", key)
	}
	if !s3KeyPattern.MatchString(key) {
		t.Errorf("key %q does not match the Phorge layout", key)
	}
}

func TestS3RequiresABucket(t *testing.T) {
	if _, err := NewS3Engine(S3Config{Region: "us-east-1"}); err == nil {
		t.Error("a bucket is required")
	}
}

func TestS3DescribesItself(t *testing.T) {
	eng, err := NewS3Engine(S3Config{Bucket: "files", Endpoint: "https://s3.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if eng.Identifier() != "amazon-s3" {
		t.Errorf("Identifier() = %q, want amazon-s3", eng.Identifier())
	}
	if eng.Priority() != 100 {
		t.Errorf("Priority() = %d, want 100", eng.Priority())
	}
	if eng.HasSizeLimit() {
		t.Error("S3 has no size limit")
	}
}

// TestS3RoundTrip drives the engine against a stand-in bucket. The endpoint is
// configurable because self-hosted Phorge points this at MinIO or Ceph, and
// that is what makes the round trip testable at all.
//
// Two things here are not incidental: the object's Content-Type comes from the
// MIME type Phorge detected, and the request carries a Content-Length. The
// second is what makes the upload a stream — without a length the SDK has to
// buffer the whole body to learn one, which is the opposite of the change this
// engine exists to support.
func TestS3RoundTrip(t *testing.T) {
	var (
		putKey         string
		putContentType string
		putLength      int64
		putBody        []byte
		deleted        string
	)

	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path-style addressing, which is why the engine sets UsePathStyle:
		// /{bucket}/{key...}
		key := strings.TrimPrefix(r.URL.Path, "/files/")

		switch r.Method {
		case http.MethodPut:
			putKey = key
			putContentType = r.Header.Get("Content-Type")
			putLength = r.ContentLength
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			putBody = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if key != putKey {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", putContentType)
			_, _ = w.Write(putBody)
		case http.MethodDelete:
			deleted = key
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer bucket.Close()

	eng, err := NewS3Engine(S3Config{
		Bucket:    "files",
		AccessKey: "AKID",
		SecretKey: "secret",
		Region:    "us-east-1",
		Endpoint:  bucket.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	const content = "hello gorge"

	// A non-seekable reader, because that is what the handler passes: an HTTP
	// request body. With a seekable one the SDK can rewind to checksum the
	// payload, so testing with strings.Reader alone would not exercise the
	// path production takes.
	handle, err := eng.WriteFile(ctx, oneByteReader{strings.NewReader(content)}, int64(len(content)),
		WriteParams{MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if handle != putKey {
		t.Errorf("the handle %q is not the key the bucket received (%q)", handle, putKey)
	}
	if putContentType != "text/plain" {
		t.Errorf("Content-Type = %q, want the MIME type Phorge gave", putContentType)
	}
	if putLength != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d", putLength, len(content))
	}

	rc, size, err := eng.ReadFile(ctx, handle)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("read back %q, want %q", got, content)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}

	if err := eng.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if deleted != handle {
		t.Errorf("deleted %q, want %q", deleted, handle)
	}
}

// TestS3ReadReportsAMissingObject: a key that is not there has to be an error
// the handler can turn into a 404, not an empty file.
func TestS3ReadReportsAMissingObject(t *testing.T) {
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bucket.Close()

	eng, err := NewS3Engine(S3Config{
		Bucket: "files", AccessKey: "AKID", SecretKey: "secret",
		Region: "us-east-1", Endpoint: bucket.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := eng.ReadFile(context.Background(), "phabricator/ab/cd/0123456789abcdef"); err == nil {
		t.Error("a missing object must be reported as an error")
	}
}
