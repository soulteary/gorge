package imagetransform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const MaxBytes = 16 << 20
const MaxPixels = 8160 * 6144

var ErrBusy = errors.New("image execution slots are busy")

type Failure struct {
	Status  int
	Code    string
	Message string
}

func (e *Failure) Error() string              { return e.Message }
func fail(status int, code, msg string) error { return &Failure{status, code, msg} }

type Info struct {
	MimeType        string `json:"mimeType"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	Frames          int    `json:"frames"`
	AnimationStatus string `json:"animationStatus"`
}
type Result struct {
	Data   []byte
	Info   Info
	Digest string
}
type Service struct {
	Binary, PolicyDir string
	Timeout           time.Duration
	slots             chan struct{}
	BackendRevision   string
	cache             resultCache
	cacheHits         atomic.Uint64
	sharedWaits       atomic.Uint64
	computations      atomic.Uint64
}

func New(binary, policy string, concurrency int, timeout time.Duration) (*Service, error) {
	if concurrency < 1 || concurrency > 8 || timeout <= 0 || timeout > 10*time.Second {
		return nil, fmt.Errorf("invalid image execution limits")
	}
	if policy == "" {
		return nil, fmt.Errorf("image policy directory is required")
	}
	s := &Service{Binary: binary, PolicyDir: policy, Timeout: timeout, slots: make(chan struct{}, concurrency)}
	raw, err := os.ReadFile(filepath.Join(policy, "policy.xml"))
	if err != nil {
		return nil, err
	}
	version, err := s.run(context.Background(), "", "-version")
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(append(raw, version...))
	s.BackendRevision = fmt.Sprintf("imagemagick-%x", digest[:8])
	var sample bytes.Buffer
	if err := png.Encode(&sample, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		return nil, err
	}
	_, err = s.withInput(sample.Bytes(), func(path string) (any, error) {
		for _, format := range []string{"png", "jpeg", "gif", "webp"} {
			out, err := s.run(context.Background(), filepath.Dir(path), path, format+":-")
			if err != nil || len(out) == 0 {
				return nil, fmt.Errorf("required image codec unavailable: %s", format)
			}
			target := filepath.Join(filepath.Dir(path), "check")
			if err = os.WriteFile(target, out, 0600); err != nil {
				return nil, err
			}
			info, err := s.probe(context.Background(), target, out)
			if err != nil || info.MimeType != "image/"+format {
				return nil, fmt.Errorf("required image codec invalid: %s", format)
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}
func (s *Service) Ready() error { _, e := s.run(context.Background(), "", "-version"); return e }

// Every request, including probe, consumes a slot. Oversubscription is refused
// rather than building an unbounded queue of compressed input bodies.
func (s *Service) withInput(data []byte, fn func(string) (any, error)) (any, error) {
	if len(data) == 0 {
		return nil, fail(422, "ERR_INVALID_IMAGE", "empty image")
	}
	if len(data) > MaxBytes {
		return nil, fail(413, "ERR_TOO_LARGE", "image exceeds 16 MiB")
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, ErrBusy
	}
	dir, err := os.MkdirTemp("", "gorge-image-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("image temporary directory cleanup failed", "error", err)
		}
	}()
	path := filepath.Join(dir, "source")
	if err = os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	return fn(path)
}
func (s *Service) Probe(ctx context.Context, data []byte) (Info, error) {
	v, e := s.withInput(data, func(path string) (any, error) { return s.probe(ctx, path, data) })
	if e != nil {
		return Info{}, e
	}
	return v.(Info), nil
}
func (s *Service) probe(ctx context.Context, path string, data []byte) (Info, error) {
	mime := ""
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		mime = "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		mime = "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		mime = "image/webp"
	default:
		return Info{}, fail(415, "ERR_UNSUPPORTED_IMAGE", "only JPEG, PNG, GIF and WebP are supported")
	}
	// Read static/ GIF canvas dimensions without pixel allocation first.
	cfg, _, headerErr := image.DecodeConfig(bytes.NewReader(data))
	if mime != "image/webp" && headerErr != nil {
		return Info{}, fail(422, "ERR_INVALID_IMAGE", "invalid image header")
	}
	if headerErr == nil && (cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxPixels/cfg.Height) {
		return Info{}, fail(413, "ERR_TOO_LARGE", "image pixel limit exceeded")
	}
	out, e := s.run(ctx, filepath.Dir(path), "-ping", path, "-format", "%m %w %h %n\n", "info:")
	if e != nil {
		return Info{}, e
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(rows[0])
	if len(fields) != 4 {
		return Info{}, fail(422, "ERR_INVALID_IMAGE", "invalid decoder metadata")
	}
	w, e1 := strconv.Atoi(fields[1])
	h, e2 := strconv.Atoi(fields[2])
	frames, e3 := strconv.Atoi(fields[3])
	if headerErr == nil {
		w, h = cfg.Width, cfg.Height
	}
	if e1 != nil || e2 != nil || e3 != nil || w < 1 || h < 1 || frames < 1 {
		return Info{}, fail(422, "ERR_INVALID_IMAGE", "invalid decoder dimensions")
	}
	if w > MaxPixels/h || frames > 100 || w > MaxPixels/h/frames {
		return Info{}, fail(413, "ERR_TOO_LARGE", "pixel or animation budget exceeded")
	}
	status := "static"
	if frames > 1 {
		status = "animated"
	}
	return Info{mime, w, h, frames, status}, nil
}
func (s *Service) compute(ctx context.Context, data []byte, recipe, animation string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	if _, ok := Recipes[recipe]; !ok {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "unknown recipe")
	}
	if animation != "legacy-static" && animation != "legacy-preserve" {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "unknown animation policy")
	}
	v, e := s.withInput(data, func(path string) (any, error) {
		info, e := s.probe(ctx, path, data)
		if e != nil {
			return nil, e
		}
		p, e := PlanTransform(info.Width, info.Height, recipe)
		if e != nil {
			return nil, fail(422, "ERR_INVALID_IMAGE", e.Error())
		}
		preserve := info.MimeType == "image/gif" && animation == "legacy-preserve" && info.Width*info.Height <= 512*512
		src := path + "[0]"
		if preserve {
			src = path
		}
		args := []string{src}
		if preserve {
			args = append(args, "-coalesce")
		}
		args = append(args, "-crop", fmt.Sprintf("%dx%d+%d+%d", p.CropWidth, p.CropHeight, p.CropX, p.CropY), "+repage", "-resize", fmt.Sprintf("%dx%d!", p.CopyWidth, p.CopyHeight), "-background", "none", "-gravity", "northwest", "-extent", fmt.Sprintf("%dx%d-%d-%d", p.Width, p.Height, p.OffsetX, p.OffsetY), "-strip")
		format := strings.TrimPrefix(info.MimeType, "image/")
		if format == "jpeg" {
			args = append(args, "-background", "white", "-alpha", "remove", "-quality", "75")
		}
		if format == "png" {
			args = append(args, "-define", "png:compression-level=6")
		}
		args = append(args, format+":-")
		out, e := s.run(ctx, filepath.Dir(path), args...)
		if e != nil {
			return nil, e
		}
		// Check encoded output through the same bounded probe before trusting it.
		target := filepath.Join(filepath.Dir(path), "output")
		if e = os.WriteFile(target, out, 0600); e != nil {
			return nil, e
		}
		verified, e := s.probe(ctx, target, out)
		if e != nil {
			return nil, e
		}
		expectedFrames := 1
		if preserve {
			expectedFrames = info.Frames
		}
		if verified.MimeType != info.MimeType || verified.Frames != expectedFrames || verified.Width != p.Width || verified.Height != p.Height {
			return nil, fmt.Errorf("decoder returned unexpected output dimensions")
		}
		digest := sha256.Sum256(out)
		return Result{out, verified, fmt.Sprintf("%x", digest)}, nil
	})
	if e != nil {
		return Result{}, e
	}
	return v.(Result), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("decoder output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func (s *Service) run(parent context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, s.Timeout)
	defer cancel()
	limits := []string{"-limit", "memory", "256MiB", "-limit", "map", "256MiB", "-limit", "disk", "256MiB", "-limit", "thread", "1", "-limit", "time", strconv.Itoa(int(s.Timeout.Seconds()))}
	if len(args) == 1 && args[0] == "-version" {
		limits = nil
	}
	cmd := exec.CommandContext(ctx, s.Binary, append(limits, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "MAGICK_CONFIGURE_PATH="+s.PolicyDir, "MAGICK_THREAD_LIMIT=1")
	if dir != "" {
		cmd.Env = append(cmd.Env, "MAGICK_TEMPORARY_PATH="+dir)
	}
	stdout := &boundedBuffer{limit: MaxBytes}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fail(504, "ERR_IMAGE_TIMEOUT", "image decoder timed out")
	}
	if err != nil {
		slog.Warn("image decoder failed", "error", err.Error(), "detail", stderr.String())
		var missing *exec.Error
		if errors.As(err, &missing) {
			return nil, fail(503, "ERR_IMAGE_UNAVAILABLE", "image decoder unavailable")
		}
		text := strings.ToLower(stderr.String())
		if strings.Contains(text, "time limit") {
			return nil, fail(504, "ERR_IMAGE_TIMEOUT", "image decoder time budget exceeded")
		}
		if strings.Contains(text, "cache resources exhausted") || strings.Contains(text, "memory allocation") || strings.Contains(text, "list length") {
			return nil, fail(413, "ERR_TOO_LARGE", "image decoder resource budget exceeded")
		}
		return nil, fail(422, "ERR_INVALID_IMAGE", "image decoder rejected input")
	}
	return stdout.Bytes(), nil
}
