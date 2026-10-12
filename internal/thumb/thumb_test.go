package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writePNG(t *testing.T, p string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{255, 0, 0, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewIsSmallJPEGAndIsCached(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	writePNG(t, p, 400, 200)
	cache := filepath.Join(dir, "cache")
	b, err := Get(context.Background(), p, cache)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil || img.Bounds().Dx() != Size || img.Bounds().Dy() != Size/2 {
		t.Fatalf("preview: %v %v", err, img.Bounds())
	}
	if e, _ := os.ReadDir(cache); len(e) != 1 {
		t.Fatalf("expected one cached preview, got %d", len(e))
	}
	os.Remove(p) // gone from the disk: the cache is keyed on the file, so a changed file is not served stale
	if _, err := Get(context.Background(), p, cache); err == nil {
		t.Fatal("a missing file must not be answered from the cache")
	}
}

func TestLargePreviewIsBiggerAndCachedApart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	writePNG(t, p, 600, 300)
	cache := filepath.Join(dir, "cache")
	small, err := Get(context.Background(), p, cache)
	if err != nil {
		t.Fatal(err)
	}
	big, err := GetSize(context.Background(), p, cache, Large)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(big))
	if err != nil || img.Bounds().Dx() != Large || img.Bounds().Dy() != Large/2 {
		t.Fatalf("large preview: %v %v", err, img.Bounds())
	}
	if len(big) <= len(small) {
		t.Fatal("the large preview should carry more detail")
	}
	if e, _ := os.ReadDir(cache); len(e) != 2 {
		t.Fatalf("each size is cached on its own, got %d files", len(e))
	}
}

func TestOtherTypesAndCancelledCallers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.bin")
	os.WriteFile(p, []byte("x"), 0o644)
	if _, err := Get(context.Background(), p, ""); err != ErrNoPreview {
		t.Fatalf("want ErrNoPreview, got %v", err)
	}
	for i := 0; i < workers; i++ {
		sem <- struct{}{}
	}
	defer func() {
		for i := 0; i < workers; i++ {
			<-sem
		}
	}()
	q := filepath.Join(dir, "b.png")
	writePNG(t, q, 10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Get(ctx, q, ""); err != context.Canceled {
		t.Fatalf("a caller that left must not wait for a worker: %v", err)
	}
}

func TestOrientationTurnsThePicture(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	got := orientate(img, 6)
	if got.Bounds().Dx() != 1 || got.Bounds().Dy() != 2 {
		t.Fatalf("orientation 6 should turn 2x1 into 1x2, got %v", got.Bounds())
	}
	if orientate(img, 1) != image.Image(img) {
		t.Fatal("orientation 1 must leave the picture alone")
	}
}

// A tiny JPEG header with an Exif segment: orientation 6, and an embedded thumbnail.
func TestExifThumbnailAndOrientation(t *testing.T) {
	var th bytes.Buffer
	jpeg.Encode(&th, image.NewRGBA(image.Rect(0, 0, 80, 60)), nil)
	le := binary.LittleEndian
	tiff := []byte("II*\x00")
	tiff = le.AppendUint32(tiff, 8)
	// IFD0: one entry (orientation = 6), next IFD at 8+2+12+4 = 26
	tiff = le.AppendUint16(tiff, 1)
	tiff = le.AppendUint16(tiff, 0x0112)
	tiff = le.AppendUint16(tiff, 3)
	tiff = le.AppendUint32(tiff, 1)
	tiff = le.AppendUint32(tiff, 6)
	tiff = le.AppendUint32(tiff, 26)
	// IFD1: offset and length of the thumbnail, which follows at 26+2+24+4 = 56
	tiff = le.AppendUint16(tiff, 2)
	tiff = le.AppendUint16(tiff, 0x0201)
	tiff = le.AppendUint16(tiff, 4)
	tiff = le.AppendUint32(tiff, 1)
	tiff = le.AppendUint32(tiff, 56)
	tiff = le.AppendUint16(tiff, 0x0202)
	tiff = le.AppendUint16(tiff, 4)
	tiff = le.AppendUint32(tiff, 1)
	tiff = le.AppendUint32(tiff, uint32(th.Len()))
	tiff = le.AppendUint32(tiff, 0)
	tiff = append(tiff, th.Bytes()...)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	file := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	file = append(file, seg...)
	file = append(file, 0xFF, 0xDA)
	got, orient := exif(file)
	if orient != 6 || !bytes.Equal(got, th.Bytes()) {
		t.Fatalf("orientation %d, thumbnail ok=%v", orient, bytes.Equal(got, th.Bytes()))
	}
	if g, o := exif([]byte("not a jpeg")); g != nil || o != 1 {
		t.Fatal("garbage must give no thumbnail and orientation 1")
	}
}
