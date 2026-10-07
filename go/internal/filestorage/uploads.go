package filestorage

// Uploads owns only resumable byte sessions. The root must be a durable local
// volume shared by processes on one host (flock + atomic rename + fsync), not
// an object-store mount or a filesystem without POSIX locking guarantees.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

const UploadChunkSize int64 = 4 << 20
const MaxUploadSize int64 = 64 << 30

var uploadID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var ErrUploadConflict = errors.New("upload conflict")
var ErrUploadExpired = errors.New("upload expired or cancelled")

type UploadChunk struct {
	Start    int64  `json:"byteStart"`
	End      int64  `json:"byteEnd"`
	Digest   string `json:"sha256,omitempty"`
	Complete bool   `json:"complete"`
}
type Upload struct {
	ID      string        `json:"id"`
	Size    int64         `json:"size"`
	Expires int64         `json:"expires"`
	State   string        `json:"state"`
	Digest  string        `json:"sha256,omitempty"`
	Cleaned bool          `json:"cleaned,omitempty"`
	Chunks  []UploadChunk `json:"chunks"`
}
type Uploads struct {
	root     string
	volumeID string
}

func NewUploads(root string) (*Uploads, error) {
	if root == "" {
		return nil, fmt.Errorf("upload root is required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	id, err := volumeIdentity(root)
	if err != nil {
		return nil, err
	}
	return &Uploads{root: root, volumeID: id}, nil
}
func (u *Uploads) lock(ctx context.Context, id string) (func(), error) {
	if !uploadID.MatchString(id) {
		return nil, ErrBadHandle
	}
	f, e := os.OpenFile(filepath.Join(u.root, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	for {
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			_ = f.Close()
			return nil, e
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
func durableWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".pending-")
	if e != nil {
		return e
	}
	name := f.Name()
	// The temporary name no longer exists after a successful rename.
	defer func() { _ = os.Remove(name) }()
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	if c := f.Close(); e == nil {
		e = c
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}
func (u *Uploads) load(id string) (*Upload, error) {
	b, e := os.ReadFile(filepath.Join(u.root, id, "manifest.json"))
	if e != nil {
		return nil, e
	}
	var s Upload
	e = json.Unmarshal(b, &s)
	if e != nil {
		return nil, e
	}
	if s.ID != id {
		return nil, fmt.Errorf("invalid upload manifest")
	}
	return &s, nil
}
func (u *Uploads) save(s *Upload) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return durableWrite(filepath.Join(u.root, s.ID, "manifest.json"), b)
}
func uploadLive(s *Upload) error {
	if s.State == "cancelled" || (s.State != "complete" && s.Expires <= time.Now().Unix()) {
		return ErrUploadExpired
	}
	return nil
}
func (u *Uploads) Create(ctx context.Context, id string, size int64) (*Upload, error) {
	if size <= 0 || size > MaxUploadSize {
		return nil, fmt.Errorf("upload size must be between 1 and %d", MaxUploadSize)
	}
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	if s, e := u.load(id); e == nil {
		if s.Size != size {
			return nil, ErrUploadConflict
		}
		return s, uploadLive(s)
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	dir := filepath.Join(u.root, id)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	if e = syncDir(u.root); e != nil {
		return nil, e
	}
	s := &Upload{ID: id, Size: size, Expires: time.Now().Add(7 * 24 * time.Hour).Unix(), State: "uploading"}
	for start := int64(0); start < size; start += UploadChunkSize {
		end := min(start+UploadChunkSize, size)
		s.Chunks = append(s.Chunks, UploadChunk{Start: start, End: end})
	}
	return s, u.save(s)
}
func (u *Uploads) Status(ctx context.Context, id string) (*Upload, error) {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	s, e := u.load(id)
	if e != nil {
		return nil, e
	}
	return s, uploadLive(s)
}
func (u *Uploads) Put(ctx context.Context, id string, start int64, data []byte) (*Upload, error) {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	s, e := u.load(id)
	if e != nil {
		return nil, e
	}
	if e = uploadLive(s); e != nil {
		return nil, e
	}
	if start < 0 || start%UploadChunkSize != 0 || start >= s.Size {
		return nil, ErrUploadConflict
	}
	chunk := &s.Chunks[start/UploadChunkSize]
	if int64(len(data)) != chunk.End-chunk.Start {
		return nil, ErrUploadConflict
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if chunk.Complete {
		if hash != chunk.Digest {
			return nil, ErrUploadConflict
		}
		return s, nil
	}
	if s.State != "uploading" {
		return nil, ErrUploadConflict
	}
	if e = durableWrite(filepath.Join(u.root, id, strconv.FormatInt(start, 10)), data); e != nil {
		return nil, e
	}
	chunk.Digest = hash
	chunk.Complete = true
	return s, u.save(s)
}
func (u *Uploads) Complete(ctx context.Context, id string) (*Upload, error) {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	s, e := u.load(id)
	if e != nil {
		return nil, e
	}
	if e = uploadLive(s); e != nil {
		return nil, e
	}
	if s.State == "complete" {
		return s, nil
	}
	digest, e := u.hashChunks(ctx, s)
	if e != nil {
		return nil, e
	}
	s.Digest = digest
	s.State = "complete"
	return s, u.save(s)
}
func (u *Uploads) Verify(ctx context.Context, id string) (*Upload, error) {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	s, e := u.load(id)
	if e != nil {
		return nil, e
	}
	if e = uploadLive(s); e != nil {
		return nil, e
	}
	if s.State != "complete" {
		return nil, ErrUploadConflict
	}
	digest, e := u.hashChunks(ctx, s)
	if e != nil {
		return nil, e
	}
	if digest != s.Digest {
		return nil, fmt.Errorf("upload file integrity mismatch")
	}
	return s, nil
}
func (u *Uploads) hashChunks(ctx context.Context, s *Upload) (string, error) {
	full := sha256.New()
	for _, c := range s.Chunks {
		if !c.Complete {
			return "", ErrUploadConflict
		}
		if e := ctx.Err(); e != nil {
			return "", e
		}
		f, e := os.Open(filepath.Join(u.root, s.ID, strconv.FormatInt(c.Start, 10)))
		if e != nil {
			return "", e
		}
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(full, h), io.LimitReader(f, c.End-c.Start+1))
		_ = f.Close()
		if e != nil {
			return "", e
		}
		if n != c.End-c.Start || hex.EncodeToString(h.Sum(nil)) != c.Digest {
			return "", fmt.Errorf("upload chunk integrity mismatch")
		}
	}
	return hex.EncodeToString(full.Sum(nil)), nil
}
func (u *Uploads) Cancel(ctx context.Context, id string) error {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	s, e := u.load(id)
	if errors.Is(e, os.ErrNotExist) {
		if e = os.MkdirAll(filepath.Join(u.root, id), 0700); e != nil {
			return e
		}
		if e = syncDir(u.root); e != nil {
			return e
		}
		return u.cancelLocked(ctx, &Upload{ID: id, State: "cancelled"})
	}
	if e != nil {
		return e
	}
	return u.cancelLocked(ctx, s)
}
func (u *Uploads) cancelLocked(ctx context.Context, s *Upload) error {
	if s.State == "cancelled" && s.Cleaned {
		return nil
	}
	// Persist the tombstone before unlinking. A crashed cancellation is replayed;
	// the same id can never resurrect a deleted file.
	s.State = "cancelled"
	if e := u.save(s); e != nil {
		return e
	}
	entries, e := os.ReadDir(filepath.Join(u.root, s.ID))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		e := os.Remove(filepath.Join(u.root, s.ID, entry.Name()))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if e := syncDir(filepath.Join(u.root, s.ID)); e != nil {
		return e
	}
	s.Cleaned = true
	return u.save(s)
}

func (u *Uploads) Read(ctx context.Context, id string, start, end int64) (io.ReadCloser, int64, error) {
	unlock, e := u.lock(ctx, id)
	if e != nil {
		return nil, 0, e
	}
	defer unlock()
	s, e := u.load(id)
	if e != nil {
		return nil, 0, e
	}
	if e = uploadLive(s); e != nil {
		return nil, 0, e
	}
	if s.State != "complete" {
		return nil, 0, ErrUploadConflict
	}
	if end < 0 {
		end = s.Size
	}
	if start < 0 || end < start || end > s.Size || end-start > UploadChunkSize {
		return nil, 0, ErrUploadConflict
	}
	var readers []io.Reader
	var files []*os.File
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, c := range s.Chunks {
		if c.End <= start || c.Start >= end {
			continue
		}
		f, e := os.Open(filepath.Join(u.root, id, strconv.FormatInt(c.Start, 10)))
		if e != nil {
			closeFiles()
			return nil, 0, e
		}
		files = append(files, f)
		hash := sha256.New()
		n, e := io.Copy(hash, io.LimitReader(f, c.End-c.Start+1))
		if e != nil || n != c.End-c.Start || hex.EncodeToString(hash.Sum(nil)) != c.Digest {
			closeFiles()
			return nil, 0, fmt.Errorf("upload chunk integrity mismatch")
		}
		a, b := max(start, c.Start)-c.Start, min(end, c.End)-c.Start
		readers = append(readers, io.NewSectionReader(f, a, b-a))
	}
	return &uploadReader{Reader: io.MultiReader(readers...), files: files}, end - start, nil
}

type uploadReader struct {
	io.Reader
	files []*os.File
}

func (r *uploadReader) Close() error {
	var errs []error
	for _, f := range r.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// Completed sessions have no automatic TTL: Phorge owns their references.
// Expired partials and interrupted cancellations are boundedly cleaned up.
func (u *Uploads) SweepExpired(ctx context.Context, limit int) error {
	entries, e := os.ReadDir(u.root)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if limit <= 0 {
			break
		}
		if !entry.IsDir() || !uploadID.MatchString(entry.Name()) {
			continue
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		unlock, e := u.lock(ctx, entry.Name())
		if e != nil {
			return e
		}
		s, e := u.load(entry.Name())
		eligible := e == nil && ((s.State == "cancelled" && !s.Cleaned) || (s.State == "uploading" && s.Expires <= time.Now().Unix()))
		if e != nil {
			unlock()
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			return e
		}
		if eligible {
			e = u.cancelLocked(ctx, s)
			limit--
		}
		unlock()
		if e != nil {
			return e
		}
	}
	return nil
}
