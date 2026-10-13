package mediafile

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Limit large image allocations independently from upstream request concurrency.
var slots = make(chan struct{}, 2)

var lanczos3 = draw.Kernel{Support: 3, At: func(x float64) float64 {
	x = math.Abs(x)
	if x == 0 {
		return 1
	}
	if x >= 3 {
		return 0
	}
	p := math.Pi * x
	return math.Sin(p) / p * math.Sin(p/3) / (p / 3)
}}

func AtomicWrite(path string, raw []byte, mode os.FileMode) error {
	return atomicWrite(path, raw, mode, false)
}

// AtomicCreate publishes a complete file without ever replacing another writer.
func AtomicCreate(path string, raw []byte, mode os.FileMode) error {
	return atomicWrite(path, raw, mode, true)
}

func atomicWrite(path string, raw []byte, mode os.FileMode, create bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".media-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if create {
		return os.Link(tmp, path)
	}
	return os.Rename(tmp, path)
}

// SamePixels accepts an existing losslessly recompressed delivery without
// overwriting it or changing its retention age.
func SamePixels(ctx context.Context, a, b []byte) bool {
	if len(a) > 64<<20 || len(b) > 64<<20 {
		return false
	}
	ca, fa, ea := image.DecodeConfig(bytes.NewReader(a))
	cb, fb, eb := image.DecodeConfig(bytes.NewReader(b))
	if ea != nil || eb != nil || fa != fb || ca.Width != cb.Width || ca.Height != cb.Height || int64(ca.Width)*int64(ca.Height) > 32<<20 {
		return false
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-slots }()
	ia, _, ea := image.Decode(bytes.NewReader(a))
	ib, _, eb := image.Decode(bytes.NewReader(b))
	if ea != nil || eb != nil {
		return false
	}
	for y := 0; y < ca.Height; y++ {
		if ctx.Err() != nil {
			return false
		}
		for x := 0; x < ca.Width; x++ {
			if color.NRGBAModel.Convert(ia.At(x, y)) != color.NRGBAModel.Convert(ib.At(x, y)) {
				return false
			}
		}
	}
	return true
}

func Dimensions(size string) (int, int, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid image dimensions")
	}
	w, ew := strconv.Atoi(parts[0])
	h, eh := strconv.Atoi(parts[1])
	if ew != nil || eh != nil || w < 1 || h < 1 || w > 8192 || h > 8192 || int64(w)*int64(h) > 32<<20 {
		return 0, 0, fmt.Errorf("image dimensions exceed limits")
	}
	return w, h, nil
}

func Transform(ctx context.Context, raw []byte, size string, thumbnail bool) ([]byte, string, error) {
	if len(raw) > 64<<20 {
		return nil, "", fmt.Errorf("image exceeds 64 MiB")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 32<<20 {
		return nil, "", fmt.Errorf("image pixel count exceeds limits")
	}
	w, h := cfg.Width, cfg.Height
	if thumbnail {
		if w > 512 || h > 512 {
			if w >= h {
				h = max(1, h*512/w)
				w = 512
			} else {
				w = max(1, w*512/h)
				h = 512
			}
		}
	} else if size != "" && size != "auto" {
		w, h, err = Dimensions(size)
		if err != nil {
			return nil, "", err
		}
		if cfg.Width >= w && cfg.Height >= h {
			return raw, "", nil
		}
	} else {
		return raw, "", nil
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-slots }()
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		return nil, "", err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	lanczos3.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	mime := "image/png"
	if thumbnail {
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80})
		mime = "image/jpeg"
	} else {
		err = png.Encode(&out, dst)
	}
	if err == nil {
		err = ctx.Err()
	}
	return out.Bytes(), mime, err
}

// RecompressPNG preserves pixels and format and only replaces a smaller result.
func RecompressPNG(ctx context.Context, raw []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if format != "png" {
		return raw, nil
	}
	if len(raw) > 64<<20 || int64(cfg.Width)*int64(cfg.Height) > 32<<20 {
		return nil, fmt.Errorf("image exceeds processing limits")
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-slots }()
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err = enc.Encode(&out, src); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if out.Len() >= len(raw) {
		return raw, nil
	}
	return out.Bytes(), nil
}
