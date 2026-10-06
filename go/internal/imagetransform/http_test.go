package imagetransform

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/gofiber/fiber/v3"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHTTPAuthAndValidation(t *testing.T) {
	s := &Service{slots: make(chan struct{}, 1)}
	app := fiber.New()
	RegisterRoutes(app, s, "secret")
	for _, c := range []struct {
		path, token string
		status      int
	}{{"/api/image/capabilities", "", 401}, {"/api/image/capabilities?token=secret", "", 401}, {"/api/image/capabilities", "secret", 200}, {"/api/image/transform?revision=wrong", "secret", 400}} {
		method := "GET"
		if c.status == 400 {
			method = "POST"
		}
		req := httptest.NewRequest(method, c.path, bytes.NewReader([]byte("abc")))
		req.Header.Set("X-Service-Token", c.token)
		res, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != c.status {
			t.Errorf("%s got %d", c.path, res.StatusCode)
		}
	}
}
func TestBusyAndByteLimit(t *testing.T) {
	s := &Service{slots: make(chan struct{}, 1)}
	s.slots <- struct{}{}
	if _, e := s.Probe(context.Background(), []byte("x")); e != ErrBusy {
		t.Fatal(e)
	}
	if _, e := s.Probe(context.Background(), make([]byte, MaxBytes+1)); e == nil {
		t.Fatal("accepted oversized body")
	}
}
func TestDeadlineKillsDecoder(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "decoder")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0700); e != nil {
		t.Fatal(e)
	}
	s := &Service{Binary: script, PolicyDir: dir, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, e := s.run(context.Background(), dir, "input")
	if e == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("unbounded execution: %v", e)
	}
}

// This suite talks to the real container; it is opt-in locally and required
// in the image-runtime CI job. It checks codecs and animation, not a mock.
func TestRealImageService(t *testing.T) {
	url := os.Getenv("GORGE_TEST_IMAGE_URL")
	if url == "" {
		t.Skip("set GORGE_TEST_IMAGE_URL for the real ImageMagick service")
	}
	token := os.Getenv("GORGE_TEST_IMAGE_TOKEN")
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(path string, body []byte, want int) []byte {
		t.Helper()
		req, e := http.NewRequest("POST", url+path, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("X-Service-Token", token)
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if err := res.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		out, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		if res.StatusCode != want {
			t.Fatalf("%s: HTTP %d: %s", path, res.StatusCode, out)
		}
		return out
	}
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 4), uint8(y * 5), 200, uint8(x * 4)})
		}
	}
	for _, format := range []string{"png", "jpeg", "gif"} {
		var b bytes.Buffer
		switch format {
		case "png":
			if err := png.Encode(&b, img); err != nil {
				t.Error(err)
				return
			}
		case "jpeg":
			if err := jpeg.Encode(&b, img, nil); err != nil {
				t.Error(err)
				return
			}
		case "gif":
			if err := gif.Encode(&b, img, nil); err != nil {
				t.Error(err)
				return
			}
		}
		for name := range Recipes {
			out := call("/api/image/transform?revision="+Revision+"&recipe="+name, b.Bytes(), 200)
			cfg, detected, e := image.DecodeConfig(bytes.NewReader(out))
			p, _ := PlanTransform(64, 48, name)
			if e != nil || detected != format || cfg.Width != p.Width || cfg.Height != p.Height {
				t.Fatalf("%s/%s invalid output: %+v %s %v", format, name, cfg, detected, e)
			}
		}
	}
	palette := color.Palette{color.Transparent, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	a := image.NewPaletted(image.Rect(0, 0, 32, 32), palette)
	b := image.NewPaletted(image.Rect(0, 0, 32, 32), palette)
	a.SetColorIndex(3, 3, 1)
	b.SetColorIndex(20, 20, 2)
	var anim bytes.Buffer
	if err := gif.EncodeAll(&anim, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{5, 12}, LoopCount: 3, Disposal: []byte{gif.DisposalBackground, gif.DisposalPrevious}}); err != nil {
		t.Error(err)
		return
	}
	out := call("/api/image/transform?revision="+Revision+"&recipe=profile&animation=legacy-preserve", anim.Bytes(), 200)
	g, e := gif.DecodeAll(bytes.NewReader(out))
	if e != nil || len(g.Image) != 2 || g.Delay[0] != 5 || g.Delay[1] != 12 || g.LoopCount != 3 {
		t.Fatalf("animation changed: %+v %v", g, e)
	}
	out = call("/api/image/transform?revision="+Revision+"&recipe=profile&animation=legacy-static", anim.Bytes(), 200)
	g, e = gif.DecodeAll(bytes.NewReader(out))
	if e != nil || len(g.Image) != 1 {
		t.Fatalf("static policy: %+v %v", g, e)
	}
	webp, e := os.ReadFile("testdata/source.webp")
	if e != nil {
		t.Fatal(e)
	}
	out = call("/api/image/transform?revision="+Revision+"&recipe=preview", webp, 200)
	call("/api/image/probe", out, 200)
	call("/api/image/probe", []byte("<svg/>"), 415)
	call("/api/image/probe", []byte("GIF89a"), 422)
	call("/api/image/transform?revision="+Revision+"&recipe=bad", webp, 400)
}

func TestOversizedCanvasRejectedBeforeDecoder(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Error(err)
		return
	}
	data := buf.Bytes()
	binary.BigEndian.PutUint32(data[16:20], 40000)
	binary.BigEndian.PutUint32(data[20:24], 40000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	s := &Service{slots: make(chan struct{}, 1), Binary: "/nonexistent/decoder"}
	_, err := s.Probe(context.Background(), data)
	f, ok := err.(*Failure)
	if !ok || f.Status != 413 {
		t.Fatalf("did not reject before decoder: %v", err)
	}
}

func TestRealImageCacheAndConcurrency(t *testing.T) {
	url := os.Getenv("GORGE_TEST_IMAGE_URL")
	if url == "" {
		t.Skip("real image service not configured")
	}
	token := os.Getenv("GORGE_TEST_IMAGE_TOKEN")
	client := &http.Client{Timeout: 20 * time.Second}
	stats := func() map[string]int {
		t.Helper()
		req, _ := http.NewRequest("GET", url+"/api/image/stats", nil)
		req.Header.Set("X-Service-Token", token)
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if err := res.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		var envelope struct{ Data map[string]int }
		if e = json.NewDecoder(res.Body).Decode(&envelope); e != nil || res.StatusCode != 200 {
			t.Fatalf("stats response: %d %v", res.StatusCode, e)
		}
		return envelope.Data
	}
	before := stats()
	var source bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 37, 29))
	img.SetNRGBA(0, 0, color.NRGBA{99, 11, 40, 255})
	if err := png.Encode(&source, img); err != nil {
		t.Error(err)
		return
	}
	var wg sync.WaitGroup
	errs := make(chan string, 24)
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("POST", url+"/api/image/transform?recipe=profile&revision="+Revision, bytes.NewReader(source.Bytes()))
			req.Header.Set("X-Service-Token", token)
			res, e := client.Do(req)
			if e != nil {
				errs <- e.Error()
				return
			}
			defer func() {
				if err := res.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if res.StatusCode != 200 {
				raw, _ := io.ReadAll(res.Body)
				errs <- string(raw)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	after := stats()
	if after["computations"]-before["computations"] > 1 {
		t.Fatalf("duplicate native computations: before=%v after=%v", before, after)
	}
	if after["cacheHits"]+after["sharedWaits"]-before["cacheHits"]-before["sharedWaits"] < 23 {
		t.Fatalf("requests not coalesced: before=%v after=%v", before, after)
	}
}
