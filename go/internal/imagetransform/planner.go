// Package imagetransform owns image computation, never Phorge file metadata.
package imagetransform

import (
	"fmt"
	"math"
)

const Revision = "phorge-v1"

type Recipe struct {
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	ScaleUp bool `json:"scaleUp"`
}

var Recipes = map[string]Recipe{
	"profile": {400, 400, true}, "pinboard": {280, 210, false},
	"thumbgrid": {100, 0, false}, "preview": {220, 0, false}, "workcard": {526, 0, true},
}

type Plan struct{ Width, Height, CropX, CropY, CropWidth, CropHeight, CopyWidth, CopyHeight, OffsetX, OffsetY int }

// PlanTransform freezes PHP's geometry, including the quarter-side canvas
// floor and no-upscale padding. Float values truncate at raster boundaries.
func PlanTransform(w, h int, recipe string) (Plan, error) {
	r, ok := Recipes[recipe]
	if !ok || w <= 0 || h <= 0 {
		return Plan{}, fmt.Errorf("invalid dimensions or recipe")
	}
	x, y := float64(w), float64(h)
	dx, dy := float64(r.Width), float64(r.Height)
	var cx, cy, ux, uy float64
	if r.Height == 0 {
		if w < h {
			dy = dx
			uy = dy
			ux = dy * x / y
			dx = math.Max(dy/4, ux)
		} else {
			ux = dx
			uy = dx * y / x
			dy = math.Max(dx/4, uy)
		}
		cx, cy = x, y
	} else {
		sx, sy := dx/x, dy/y
		if !r.ScaleUp {
			sx = math.Min(sx, 1)
			sy = math.Min(sy, 1)
		}
		scale := math.Max(sx, sy)
		cx, cy = dx/scale, dy/scale
		if !r.ScaleUp {
			cx = math.Min(x, cx)
			cy = math.Min(y, cy)
		}
		ux, uy = dx, dy
	}
	px, py := math.Min(dx, ux), math.Min(dy, uy)
	if !r.ScaleUp {
		px = math.Min(px, cx)
		py = math.Min(py, cy)
	}
	p := Plan{int(dx), int(dy), int((x - cx) / 2), int((y - cy) / 2), int(cx), int(cy), int(px), int(py), int((dx - px) / 2), int((dy - py) / 2)}
	if p.CopyWidth < 1 || p.CopyHeight < 1 || p.CropWidth < 1 || p.CropHeight < 1 {
		return Plan{}, fmt.Errorf("image aspect ratio produces an empty raster")
	}
	return p, nil
}
