package imagetransform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
)

const ComposeRevision = "compose-v1"

type ComposeRequest struct {
	Recipe     string    `json:"recipe"`
	Revision   string    `json:"revision"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Background string    `json:"background"`
	Border     []float64 `json:"border"`
	Layers     [][]byte  `json:"layers"`
}

func (s *Service) Compose(ctx context.Context, r ComposeRequest) (Result, error) {
	if r.Revision != ComposeRevision {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid compose revision")
	}
	w, h := r.Width, r.Height
	switch r.Recipe {
	case "avatar":
		w, h = 400, 400
	case "icon":
		w, h = 200, 200
	case "favicon":
		if w != h || (w != 16 && w != 32 && w != 64 && w != 128) {
			return Result{}, fail(400, "ERR_BAD_REQUEST", "unsupported favicon dimensions")
		}
	default:
		return Result{}, fail(400, "ERR_BAD_REQUEST", "unknown compose recipe")
	}
	if len(r.Layers) == 0 || len(r.Layers) > 5 || ((r.Recipe == "avatar" || r.Recipe == "icon") && len(r.Layers) != 1) {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid compose layers")
	}
	colorBytes, e := hex.DecodeString(strings.TrimPrefix(r.Background, "#"))
	if e != nil || len(colorBytes) != 3 {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid background color")
	}
	if r.Recipe == "avatar" {
		if len(r.Border) != 4 {
			return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid avatar border")
		}
		for i, v := range r.Border {
			limit := float64(255)
			if i == 3 {
				limit = 1
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > limit {
				return Result{}, fail(400, "ERR_BAD_REQUEST", "invalid avatar border")
			}
		}
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Result{}, ErrBusy
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if r.Recipe != "favicon" {
		draw.Draw(dst, dst.Bounds(), image.NewUniform(color.RGBA{colorBytes[0], colorBytes[1], colorBytes[2], 255}), image.Point{}, draw.Src)
	}
	if r.Recipe == "avatar" {
		border := image.NewUniform(color.NRGBA{uint8(r.Border[0]), uint8(r.Border[1]), uint8(r.Border[2]), uint8(math.Round(255 * r.Border[3]))})
		for _, rect := range []image.Rectangle{image.Rect(0, 0, w, 32), image.Rect(0, h-32, w, h), image.Rect(0, 32, 32, h-32), image.Rect(w-32, 32, w, h-32)} {
			draw.Draw(dst, rect, border, image.Point{}, draw.Over)
		}
	}
	bytesTotal, pixelsTotal := 0, 0
	for i, data := range r.Layers {
		if e = ctx.Err(); e != nil {
			return Result{}, e
		}
		if len(data) == 0 {
			if i == 0 {
				return Result{}, fail(422, "ERR_INVALID_IMAGE", "empty base layer")
			}
			continue
		}
		bytesTotal += len(data)
		if bytesTotal > 8<<20 {
			return Result{}, fail(413, "ERR_TOO_LARGE", "compose input exceeds 8 MiB")
		}
		cfg, format, e := image.DecodeConfig(bytes.NewReader(data))
		if e != nil || (format != "png" && format != "jpeg" && format != "gif" && format != "webp") {
			return Result{}, fail(422, "ERR_INVALID_IMAGE", "invalid compose image")
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MemeMaxPixels/cfg.Height {
			return Result{}, fail(413, "ERR_TOO_LARGE", "compose pixel budget exceeded")
		}
		pixelsTotal += cfg.Width * cfg.Height
		if pixelsTotal > MemeMaxPixels {
			return Result{}, fail(413, "ERR_TOO_LARGE", "compose pixel budget exceeded")
		}
		src, _, e := image.Decode(bytes.NewReader(data))
		if e != nil {
			return Result{}, fail(422, "ERR_INVALID_IMAGE", "invalid compose image")
		}
		if r.Recipe != "favicon" {
			draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
			continue
		}
		rect := dst.Bounds()
		if i > 0 {
			a := w / 2
			b := h / 2
			switch i {
			case 1:
				rect = image.Rect(w-a, 0, w, b)
			case 2:
				rect = image.Rect(w-a, h-b, w, h)
			case 3:
				rect = image.Rect(0, h-b, a, h)
			case 4:
				rect = image.Rect(0, 0, a, b)
			}
		}
		xdraw.CatmullRom.Scale(dst, rect, src, src.Bounds(), draw.Over, nil)
	}
	var b bytes.Buffer
	if e = png.Encode(&b, dst); e != nil {
		return Result{}, e
	}
	hash := sha256.Sum256(b.Bytes())
	s.computations.Add(1)
	return Result{Data: b.Bytes(), Info: Info{MimeType: "image/png", Width: w, Height: h, Frames: 1, AnimationStatus: "static"}, Digest: fmt.Sprintf("%x", hash)}, nil
}
