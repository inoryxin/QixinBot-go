package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xin/onebot/internal/onebot"
)

func TestSegmentImagesMakesInlineThumbnail(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 40, B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	segs := []onebot.Segment{{Type: "image", Data: map[string]any{"file": "base64://" + base64.StdEncoding.EncodeToString(buf.Bytes())}}}
	got := segmentImages(segs, t.TempDir())
	if len(got) != 1 || !strings.HasPrefix(got[0], "base64://") {
		t.Fatalf("got %#v", got)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got[0], "base64://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("thumbnail should decode: %v", err)
	}
}

func TestSegmentImagesBadInlineFallsBack(t *testing.T) {
	got := segmentImages([]onebot.Segment{{Type: "image", Data: map[string]any{"file": "base64://not-image"}}}, t.TempDir())
	if len(got) != 1 || got[0] != "（内联图片）" {
		t.Fatalf("got %#v", got)
	}
}

func TestSegmentImagesSupportsFileAndData(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{path, "file:///" + strings.TrimPrefix(path, "/"), "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())} {
		got := segmentImages([]onebot.Segment{{Type: "image", Data: map[string]any{"file": src}}}, dir)
		if len(got) != 1 || !strings.HasPrefix(got[0], "base64://") {
			t.Fatalf("src=%q got=%#v", src, got)
		}
	}
	// dataRoot 外的路径不能读
	if got := segmentImages([]onebot.Segment{{Type: "image", Data: map[string]any{"file": filepath.Join(t.TempDir(), "x.png")}}}, dir); len(got) != 1 || got[0] != "（内联图片）" {
		t.Fatalf("outside path got=%#v", got)
	}
}
