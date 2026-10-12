// Package thumb makes the small previews shown next to photos. It is built to stay cheap: a JPEG
// that carries its own EXIF thumbnail is never fully decoded, huge pictures are refused, only a
// few run at once, and every result is kept in a cache keyed by path, size and modification time.
package thumb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Size is the longest side of a preview, in pixels. The page shows it at half this, for sharp screens.
const Size = 128

const (
	maxFileBytes = 64 << 20 // bigger files are not decoded
	maxPixels    = 50_000_000
	workers      = 3
)

// ErrNoPreview means this file has no preview: the type is not handled, or it is too big to be cheap.
var ErrNoPreview = errors.New("no preview for this file")

var sem = make(chan struct{}, workers)

// CanPreview tells, from the extension alone (lower case, no dot), whether a preview can be made.
func CanPreview(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tif", "tiff":
		return true
	}
	return false
}

// Get returns a JPEG preview of the file at path. cacheDir may be empty to skip the cache. It
// waits for a free worker, and gives up with ctx.Err() if the caller leaves first.
func Get(ctx context.Context, path, cacheDir string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxFileBytes {
		return nil, ErrNoPreview
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if !CanPreview(ext) {
		return nil, ErrNoPreview
	}
	var cached string
	if cacheDir != "" {
		h := sha1.Sum([]byte(path + "|" + st.ModTime().UTC().String() + "|" + strconv.FormatInt(st.Size(), 10)))
		cached = filepath.Join(cacheDir, hex.EncodeToString(h[:])+".jpg")
		if b, err := os.ReadFile(cached); err == nil {
			return b, nil
		}
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b, err := make1(path, ext)
	if err != nil {
		return nil, err
	}
	if cached != "" && os.MkdirAll(cacheDir, 0o755) == nil {
		tmp := cached + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, cached)
		}
	}
	return b, nil
}

func make1(path, ext string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var img image.Image
	orient := 1
	if ext == "jpg" || ext == "jpeg" {
		head, err := io.ReadAll(io.LimitReader(f, 256<<10)) // EXIF lives in the first segments
		if err != nil {
			return nil, err
		}
		var thumb []byte
		thumb, orient = exif(head)
		if thumb != nil {
			if t, err := jpeg.Decode(bytes.NewReader(thumb)); err == nil && t.Bounds().Dx() >= 64 && t.Bounds().Dy() >= 64 {
				img = t
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if img == nil {
		cfg, _, err := image.DecodeConfig(f)
		if err != nil {
			return nil, err
		}
		if cfg.Width*cfg.Height > maxPixels {
			return nil, ErrNoPreview
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if ext == "gif" {
			g, err := gif.Decode(f) // first frame only
			if err != nil {
				return nil, err
			}
			img = g
		} else if img, _, err = image.Decode(f); err != nil {
			return nil, err
		}
	}
	return encode(orientate(shrink(img), orient))
}

func shrink(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= Size && h <= Size {
		return src
	}
	if w >= h {
		h = max(1, h*Size/w)
		w = Size
	} else {
		w = max(1, w*Size/h)
		h = Size
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func encode(img image.Image) ([]byte, error) {
	// A picture with transparency is flattened on white: JPEG has no alpha.
	b := img.Bounds()
	flat := image.NewRGBA(b)
	draw.Draw(flat, b, image.White, image.Point{}, draw.Src)
	draw.Draw(flat, b, img, b.Min, draw.Over)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, flat, &jpeg.Options{Quality: 78}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// orientate applies the EXIF orientation (1 = as is) so previews stand upright.
func orientate(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if o >= 5 {
		nw, nh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// exif reads the embedded thumbnail (nil if none) and the orientation (1 if none) from the
// start of a JPEG file.
func exif(d []byte) (thumb []byte, orient int) {
	orient = 1
	if len(d) < 4 || d[0] != 0xFF || d[1] != 0xD8 {
		return
	}
	i := 2
	for i+4 <= len(d) && d[i] == 0xFF {
		marker, n := d[i+1], int(binary.BigEndian.Uint16(d[i+2:]))
		if marker == 0xDA || n < 2 { // start of image data
			return
		}
		if marker == 0xE1 && i+2+n <= len(d) && bytes.HasPrefix(d[i+4:i+2+n], []byte("Exif\x00\x00")) {
			return parseTIFF(d[i+10 : i+2+n])
		}
		i += 2 + n
	}
	return
}

func parseTIFF(t []byte) (thumb []byte, orient int) {
	orient = 1
	if len(t) < 8 {
		return
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}
	// entries reads one image file directory: its tag values, and the offset of the next one.
	entries := func(off int) (map[uint16]uint32, int) {
		if off < 8 || off+2 > len(t) {
			return nil, 0
		}
		n := int(bo.Uint16(t[off:]))
		if off+2+n*12+4 > len(t) {
			return nil, 0
		}
		m := map[uint16]uint32{}
		for k := 0; k < n; k++ {
			e := t[off+2+k*12:]
			typ := bo.Uint16(e[2:])
			if typ == 3 { // SHORT: the value sits in the first two bytes
				m[bo.Uint16(e)] = uint32(bo.Uint16(e[8:]))
			} else {
				m[bo.Uint16(e)] = bo.Uint32(e[8:])
			}
		}
		return m, int(bo.Uint32(t[off+2+n*12:]))
	}
	ifd0, next := entries(int(bo.Uint32(t[4:])))
	if o, ok := ifd0[0x0112]; ok && o >= 1 && o <= 8 {
		orient = int(o)
	}
	if ifd1, _ := entries(next); ifd1 != nil {
		off, ln := int(ifd1[0x0201]), int(ifd1[0x0202])
		if off > 0 && ln > 0 && off+ln <= len(t) {
			thumb = t[off : off+ln]
		}
	}
	return
}
