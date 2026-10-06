package imagetransform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func pngLayer(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestComposeRecipesAndCorners(t *testing.T) {
	s := &Service{slots: make(chan struct{}, 1)}
	base := pngLayer(t, 1, 1, color.NRGBA{A: 0})
	red := pngLayer(t, 1, 1, color.NRGBA{R: 255, A: 255})
	r, e := s.Compose(context.Background(), ComposeRequest{Recipe: "avatar", Revision: ComposeRevision, Background: "#00ff00", Border: []float64{255, 0, 0, 1}, Layers: [][]byte{base}})
	if e != nil {
		t.Fatal(e)
	}
	img, e := png.Decode(bytes.NewReader(r.Data))
	if e != nil {
		t.Fatal(e)
	}
	if img.Bounds().Dx() != 400 {
		t.Fatal(img.Bounds())
	}
	corner := color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA)
	center := color.NRGBAModel.Convert(img.At(200, 200)).(color.NRGBA)
	if corner.R != 255 || center.G != 255 {
		t.Fatal(corner, center)
	}
	r, e = s.Compose(context.Background(), ComposeRequest{Recipe: "favicon", Revision: ComposeRevision, Width: 64, Height: 64, Background: "#00ff00", Layers: [][]byte{base, nil, red, nil, nil}})
	if e != nil {
		t.Fatal(e)
	}
	img, _ = png.Decode(bytes.NewReader(r.Data))
	if color.NRGBAModel.Convert(img.At(48, 48)).(color.NRGBA).R != 255 || color.NRGBAModel.Convert(img.At(48, 16)).(color.NRGBA).A != 0 {
		t.Fatal("emblem corner order or favicon transparency")
	}
}
func TestComposeRejectsUnsupportedGeometryAndBudgets(t *testing.T) {
	s := &Service{slots: make(chan struct{}, 1)}
	for _, r := range []ComposeRequest{{Recipe: "freeform", Revision: ComposeRevision}, {Recipe: "favicon", Revision: ComposeRevision, Width: 1000000, Height: 1000000}, {Recipe: "avatar", Revision: ComposeRevision, Background: "bad", Layers: [][]byte{{1}}}} {
		if _, e := s.Compose(context.Background(), r); e == nil {
			t.Fatal("invalid composition accepted")
		}
	}
}
func TestMemeFontAndLiteralText(t *testing.T) {
	s := &Service{}
	if e := s.SetMemeFont(nil); e != nil {
		t.Fatal(e)
	}
	if len(s.MemeFontRevision) != 64 {
		t.Fatal("missing font revision")
	}
	data, e := memeOverlay(s.MemeFont, 400, 400, "@literal %value 'quote'", "two\nlines")
	if e != nil {
		t.Fatal(e)
	}
	img, e := png.Decode(bytes.NewReader(data))
	if e != nil || img.Bounds().Dx() != 400 {
		t.Fatal(e)
	}
	if _, e = memeOverlay(s.MemeFont, 1, 1, "does not fit", ""); e == nil {
		t.Fatal("overflow text accepted")
	}
	if e = validateMemeText("\x00", ""); e == nil {
		t.Fatal("control accepted")
	}
	if e = validateMemeText(string([]byte{0xff}), ""); e == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if e = s.SetMemeFont([]byte("bad")); e == nil {
		t.Fatal("invalid font accepted")
	}
}

func TestRealMemeAndCompose(t *testing.T) {
	url := os.Getenv("GORGE_TEST_IMAGE_URL")
	if url == "" {
		t.Skip("set GORGE_TEST_IMAGE_URL")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(path string, body []byte, above string) []byte {
		t.Helper()
		req, e := http.NewRequest("POST", url+path, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("X-Service-Token", os.Getenv("GORGE_TEST_IMAGE_TOKEN"))
		req.Header.Set("X-Gorge-Meme-Above", base64.StdEncoding.EncodeToString([]byte(above)))
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = res.Body.Close() }()
		out, e := io.ReadAll(res.Body)
		if e != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %d %s %v", path, res.StatusCode, out, e)
		}
		hash := sha256.Sum256(out)
		if res.Header.Get("ETag") != fmt.Sprintf("\"%x\"", hash) {
			t.Fatal("digest header")
		}
		return out
	}
	source := pngLayer(t, 400, 400, color.NRGBA{R: 120, G: 120, B: 120, A: 255})
	out := call("/api/image/meme?revision=meme-v1", source, "literal @file %[value]")
	img, _, e := image.Decode(bytes.NewReader(out))
	if e != nil || img.Bounds().Dx() != 400 || bytes.Equal(out, source) {
		t.Fatal("invalid meme output", e)
	}
	caption := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"\n", 16), "\n")
	out = call("/api/image/meme?revision=meme-v1", pngLayer(t, 2000, 600, color.NRGBA{A: 255}), caption)
	img, _, e = image.Decode(bytes.NewReader(out))
	if e != nil || img.Bounds().Dx() != 2000 {
		t.Fatal("large caption header rejected", e)
	}
	palette := color.Palette{color.Black, color.White}
	a := image.NewPaletted(image.Rect(0, 0, 200, 200), palette)
	b := image.NewPaletted(a.Bounds(), palette)
	for i := range b.Pix {
		b.Pix[i] = 1
	}
	var animated bytes.Buffer
	original := &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{7, 11}, LoopCount: 3}
	if e = gif.EncodeAll(&animated, original); e != nil {
		t.Fatal(e)
	}
	out = call("/api/image/meme?revision=meme-v1&animation=legacy-preserve", animated.Bytes(), "animated")
	g, e := gif.DecodeAll(bytes.NewReader(out))
	if e != nil || len(g.Image) != 2 || g.Delay[0] != 7 || g.Delay[1] != 11 || g.LoopCount != 3 {
		t.Fatalf("animation changed: %+v %v", g, e)
	}
	request := ComposeRequest{Recipe: "icon", Revision: ComposeRevision, Background: "#ff0000", Layers: [][]byte{pngLayer(t, 200, 200, color.NRGBA{})}}
	raw, e := json.Marshal(request)
	if e != nil {
		t.Fatal(e)
	}
	out = call("/api/image/compose", raw, "")
	img, _, e = image.Decode(bytes.NewReader(out))
	if e != nil || img.Bounds().Dx() != 200 {
		t.Fatal(e)
	}
	request = ComposeRequest{Recipe: "favicon", Revision: ComposeRevision, Width: 64, Height: 64, Background: "#000000", Layers: [][]byte{pngLayer(t, 1, 1, color.NRGBA{}), pngLayer(t, 1, 1, color.NRGBA{R: 255, A: 255})}}
	raw, e = json.Marshal(request)
	if e != nil {
		t.Fatal(e)
	}
	out = call("/api/image/compose", raw, "")
	img, _, e = image.Decode(bytes.NewReader(out))
	if e != nil || color.NRGBAModel.Convert(img.At(48, 16)).(color.NRGBA).R != 255 || color.NRGBAModel.Convert(img.At(16, 48)).(color.NRGBA).A != 0 {
		t.Fatal("favicon transparency or corner changed", e)
	}
}
