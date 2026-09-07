package filestorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// identifierS3 is what Phorge records against every file this backend writes.
const identifierS3 = "amazon-s3"

// S3Engine stores each file as one object.
type S3Engine struct {
	client       *s3.Client
	bucket       string
	instanceName string
}

// S3Config is what the S3 backend needs. All of it is required except
// InstanceName; Config.S3Enabled is what enforces that.
type S3Config struct {
	Bucket       string
	AccessKey    string
	SecretKey    string
	Region       string
	Endpoint     string
	InstanceName string
}

// NewS3Engine builds the client. Nothing is dialled here, so a wrong endpoint
// surfaces on the first write rather than at startup.
//
// UsePathStyle is on because the endpoint is explicit: MinIO, Ceph and the
// other S3-compatible services a self-hosted Phorge points at do not serve
// virtual-hosted style buckets.
func NewS3Engine(cfg S3Config) (*S3Engine, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}

	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,
	})

	return &S3Engine{
		client:       client,
		bucket:       cfg.Bucket,
		instanceName: cfg.InstanceName,
	}, nil
}

func (e *S3Engine) Identifier() string { return identifierS3 }

// Priority is the highest of the three, so a deployment with object storage
// still prefers the database for small files and the local disk for the rest.
// Object storage wins only when it is the sole backend configured.
func (e *S3Engine) Priority() int      { return 100 }
func (e *S3Engine) CanWrite() bool     { return true }
func (e *S3Engine) HasSizeLimit() bool { return false }
func (e *S3Engine) MaxFileSize() int64 { return 0 }

// unsignedPayload lets PutObject stream a body it cannot rewind.
//
// SigV4 normally covers the payload with a SHA256 the signer computes by
// reading the body and seeking back to the start — and an HTTP request body
// does not seek, so that attempt fails outright with "request stream is not
// seekable". The SDK already avoids it over HTTPS, where it declares the
// payload unsigned instead; this extends the same choice to a plain-HTTP
// endpoint, which is what a self-hosted MinIO or Ceph is usually reached
// over. It is applied to PutObject alone, since it is the only operation here
// carrying a body.
//
// Without it the alternative is buffering every upload to compute the hash,
// which is the thing streaming was introduced to stop.
func unsignedPayload(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
}

// WriteFile streams src into one object.
//
// The length is what makes it a stream rather than a buffer: with
// ContentLength set the SDK sends the body as it reads it, and without one it
// would have to buffer the whole body to learn its length. That is the reason
// the handler insists on a Content-Length.
func (e *S3Engine) WriteFile(ctx context.Context, src io.Reader, size int64, params WriteParams) (string, error) {
	key, err := e.generateKey()
	if err != nil {
		return "", err
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(e.bucket),
		Key:    aws.String(key),
		Body:   src,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	// The MIME type Phorge detected, recorded on the object. Nothing in this
	// service reads it back — the read endpoint always answers
	// application/octet-stream — but it is what makes an object served
	// straight from the bucket, or inspected in a console, not arrive as a
	// download of unknown type.
	if params.MimeType != "" {
		input.ContentType = aws.String(params.MimeType)
	}

	if _, err := e.client.PutObject(ctx, input, unsignedPayload); err != nil {
		return "", fmt.Errorf("s3 put: %w", err)
	}
	return key, nil
}

// ReadFile opens the object and reports the length S3 gave for it.
func (e *S3Engine) ReadFile(ctx context.Context, handle string) (io.ReadCloser, int64, error) {
	out, err := e.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(e.bucket),
		Key:    aws.String(handle),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("s3 get: %w", err)
	}

	size := int64(-1)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// DeleteFile removes the object. S3 already treats deleting a key that is not
// there as a success, which is the behaviour StorageEngine.DeleteFile asks for.
func (e *S3Engine) DeleteFile(ctx context.Context, handle string) error {
	_, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(e.bucket),
		Key:    aws.String(handle),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	return nil
}

// generateKey mints `phabricator[/instance]/ab/cd/{16 hex}`.
//
// The `phabricator` prefix is not a typo and not decoration: it is the prefix
// Phorge's own S3 engine uses, so a bucket written by one is readable by the
// other. Changing it leaves every existing object in place and unreachable.
// See compat/phorge/README.md section 7.
func (e *S3Engine) generateKey() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random key: %w", err)
	}
	seed := hex.EncodeToString(b)

	key := "phabricator"
	if e.instanceName != "" {
		key += "/" + e.instanceName
	}
	key += fmt.Sprintf("/%s/%s/%s", seed[:2], seed[2:4], seed[4:])
	return key, nil
}
