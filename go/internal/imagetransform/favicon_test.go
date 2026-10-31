package imagetransform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestNativeFaviconCompositionSizes(t *testing.T) {
	base := pngLayer(t, 8, 8, color.NRGBA{})
	emblem := pngLayer(t, 1, 1, color.NRGBA{R: 255, A: 255})
	for _, size := range []int{16, 32, 64, 76, 120, 128, 152} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := &Service{slots: make(chan struct{}, 1)}
			r, err := s.Compose(context.Background(), ComposeRequest{
				Recipe: "favicon", Revision: ComposeRevision, Width: size, Height: size,
				Background: "#000000", Layers: [][]byte{base, emblem},
			})
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(r.Data))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != size || img.Bounds().Dy() != size || r.Info.Width != size || r.Info.Height != size {
				t.Fatalf("wrong output dimensions: %v, %+v", img.Bounds(), r.Info)
			}
			if color.NRGBAModel.Convert(img.At(3*size/4, size/4)).(color.NRGBA).R != 255 ||
				color.NRGBAModel.Convert(img.At(size/4, 3*size/4)).(color.NRGBA).A != 0 {
				t.Fatal("favicon emblem placement or transparency changed")
			}
			if r.Digest != fmt.Sprintf("%x", sha256.Sum256(r.Data)) {
				t.Fatal("output digest does not describe the PNG")
			}
		})
	}
}

func TestNativeFaviconHTTPDimensionsAndBounds(t *testing.T) {
	app := fiber.New()
	RegisterRoutes(app, &Service{slots: make(chan struct{}, 1)}, "favicon-test")
	for _, tc := range []struct{ w, h, status int }{
		{16, 16, 200}, {32, 32, 200}, {64, 64, 200}, {76, 76, 200},
		{120, 120, 200}, {128, 128, 200}, {152, 152, 200},
		{0, 0, 400}, {-1, -1, 400}, {75, 75, 400}, {77, 77, 400},
		{119, 119, 400}, {121, 121, 400}, {151, 151, 400}, {153, 153, 400},
		{76, 120, 400}, {1000000, 1000000, 400},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			body, err := json.Marshal(ComposeRequest{Recipe: "favicon", Revision: ComposeRevision,
				Width: tc.w, Height: tc.h, Background: "#000000",
				Layers: [][]byte{pngLayer(t, 1, 1, color.NRGBA{A: 255})}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/image/compose", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Service-Token", "favicon-test")
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := res.Body.Close(); err != nil {
					t.Errorf("close response body: %v", err)
				}
			}()
			if res.StatusCode != tc.status {
				t.Fatalf("got HTTP %d, want %d", res.StatusCode, tc.status)
			}
			if tc.status != 200 {
				return
			}
			data, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width != tc.w || cfg.Height != tc.h {
				t.Fatalf("wrong PNG geometry: %+v, %v", cfg, err)
			}
			if res.Header.Get("ETag") != fmt.Sprintf("\"%x\"", sha256.Sum256(data)) ||
				res.Header.Get("X-Gorge-Recipe-Revision") != ComposeRevision || res.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("composition response contract changed")
			}
		})
	}
}
