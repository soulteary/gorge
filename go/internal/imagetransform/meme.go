package imagetransform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MemeRevision = "meme-v1"
const MemeMaxTextBytes = 4096
const MemeMaxPixels = 16 << 20

func (s *Service) SetMemeFont(data []byte) error {
	if len(data) == 0 {
		data = gobold.TTF
	}
	if len(data) > 8<<20 {
		return fmt.Errorf("meme font exceeds 8 MiB")
	}
	f, e := opentype.Parse(data)
	if e != nil {
		return e
	}
	digest := sha256.Sum256(data)
	s.MemeFont = f
	s.MemeFontRevision = fmt.Sprintf("%x", digest[:])
	return nil
}
func validateMemeText(above, below string) error {
	if len(above)+len(below) > MemeMaxTextBytes || !utf8.ValidString(above) || !utf8.ValidString(below) {
		return fail(400, "ERR_BAD_REQUEST", "meme text exceeds limits or is not UTF-8")
	}
	for _, text := range []string{above, below} {
		if len(strings.Split(text, "\n")) > 16 {
			return fail(400, "ERR_BAD_REQUEST", "too many meme text lines")
		}
		for _, r := range text {
			if unicode.IsControl(r) && r != '\n' {
				return fail(400, "ERR_BAD_REQUEST", "unsupported meme control character")
			}
		}
	}
	return nil
}
func memeFace(f *opentype.Font, size int) (font.Face, error) {
	return opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
}
func textBounds(face font.Face, text string) (int, int) {
	width := 0
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		bounds, _ := font.BoundString(face, line)
		width = max(width, (bounds.Max.X - bounds.Min.X).Ceil())
	}
	return width, len(lines) * face.Metrics().Height.Ceil()
}
func memeOverlay(f *opentype.Font, w, h int, above, below string) ([]byte, error) {
	if w <= 0 || h <= 0 || w > MemeMaxPixels/h {
		return nil, fail(413, "ERR_TOO_LARGE", "meme canvas pixel limit exceeded")
	}
	var face font.Face
	for size := 72; size >= 5; size-- {
		candidate, e := memeFace(f, size)
		if e != nil {
			return nil, e
		}
		fits := true
		for _, text := range []string{above, below} {
			if strings.TrimSpace(text) == "" {
				continue
			}
			tw, th := textBounds(candidate, text)
			if tw+16 > w || th+16 > h {
				fits = false
			}
		}
		if fits {
			face = candidate
			break
		}
		_ = candidate.Close()
	}
	if face == nil {
		return nil, fail(422, "ERR_INVALID_IMAGE", "meme text does not fit the canvas")
	}
	defer func() { _ = face.Close() }()
	for _, text := range []string{above, below} {
		for _, r := range text {
			if r == '\n' {
				continue
			}
			if _, ok := face.GlyphAdvance(r); !ok {
				return nil, fail(422, "ERR_UNSUPPORTED_TEXT", "configured meme font does not contain a requested glyph")
			}
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	lineHeight := face.Metrics().Height.Ceil()
	ascent := face.Metrics().Ascent.Ceil()
	draw := func(text string, bottom bool) {
		if strings.TrimSpace(text) == "" {
			return
		}
		lines := strings.Split(text, "\n")
		y := 12 + ascent
		if bottom {
			y = h - 12 - len(lines)*lineHeight + ascent
		}
		for _, line := range lines {
			bounds, _ := font.BoundString(face, line)
			x := (w-(bounds.Max.X-bounds.Min.X).Ceil())/2 - bounds.Min.X.Floor()
			drawer := font.Drawer{Dst: img, Face: face, Src: image.NewUniform(color.Black)}
			for dx := -1; dx <= 1; dx++ {
				for dy := -1; dy <= 1; dy++ {
					drawer.Dot = fixed.P(x+dx, y+dy)
					drawer.DrawString(line)
				}
			}
			drawer.Src = image.NewUniform(color.White)
			drawer.Dot = fixed.P(x, y)
			drawer.DrawString(line)
			y += lineHeight
		}
	}
	draw(above, false)
	draw(below, true)
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func (s *Service) Meme(ctx context.Context, data []byte, above, below, animation string) (Result, error) {
	if e := validateMemeText(above, below); e != nil {
		return Result{}, e
	}
	if s.MemeFont == nil {
		return Result{}, fail(503, "ERR_IMAGE_UNAVAILABLE", "meme font unavailable")
	}
	if animation != "legacy-static" && animation != "legacy-preserve" {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid animation policy")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	v, e := s.withInput(data, func(path string) (any, error) {
		info, e := s.probe(ctx, path, data)
		if e != nil {
			return nil, e
		}
		if info.Width > MemeMaxPixels/info.Height || info.Frames > 0 && info.Width > MaxPixels/info.Height/info.Frames {
			return nil, fail(413, "ERR_TOO_LARGE", "meme frame pixel budget exceeded")
		}
		overlay, e := memeOverlay(s.MemeFont, info.Width, info.Height, above, below)
		if e != nil {
			return nil, e
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		overlayPath := filepath.Join(filepath.Dir(path), "overlay.png")
		if e = os.WriteFile(overlayPath, overlay, 0600); e != nil {
			return nil, e
		}
		preserve := info.MimeType == "image/gif" && animation == "legacy-preserve"
		src := path + "[0]"
		expected := 1
		if preserve {
			src = path
			expected = info.Frames
		}
		args := []string{src}
		if preserve {
			args = append(args, "-coalesce")
		}
		// Only a server-generated path enters the drawing expression. User text is
		// rasterized in Go and cannot invoke ImageMagick properties or file reads.
		args = append(args, "-draw", fmt.Sprintf("image over 0,0 0,0 '%s'", overlayPath), "-strip")
		format := strings.TrimPrefix(info.MimeType, "image/")
		if format == "jpeg" {
			args = append(args, "-quality", "90")
		}
		args = append(args, format+":-")
		out, e := s.run(ctx, filepath.Dir(path), args...)
		if e != nil {
			return nil, e
		}
		target := filepath.Join(filepath.Dir(path), "output")
		if e = os.WriteFile(target, out, 0600); e != nil {
			return nil, e
		}
		verified, e := s.probe(ctx, target, out)
		if e != nil {
			return nil, e
		}
		if verified.Width != info.Width || verified.Height != info.Height || verified.MimeType != info.MimeType || verified.Frames != expected {
			return nil, fmt.Errorf("invalid meme decoder output")
		}
		digest := sha256.Sum256(out)
		s.computations.Add(1)
		return Result{out, verified, fmt.Sprintf("%x", digest)}, nil
	})
	if e != nil {
		return Result{}, e
	}
	return v.(Result), nil
}
