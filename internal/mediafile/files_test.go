package mediafile

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestTransformThumbnailDimensionsAndLosslessCompression(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1200, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 1200; x++ {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var encoded bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&encoded, src); err != nil {
		t.Fatal(err)
	}
	thumb, mime, err := Transform(context.Background(), encoded.Bytes(), "", true)
	if err != nil || mime != "image/jpeg" {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(thumb))
	if err != nil || cfg.Width != 512 || cfg.Height != 256 {
		t.Fatalf("thumbnail dimensions: %#v %v", cfg, err)
	}
	compressed, err := RecompressPNG(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) >= encoded.Len() {
		t.Fatal("uncompressed PNG was not compressed")
	}
	restored, _, err := image.Decode(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []image.Point{{0, 0}, {701, 303}, {1199, 599}} {
		if color.NRGBAModel.Convert(restored.At(p.X, p.Y)) != color.NRGBAModel.Convert(src.At(p.X, p.Y)) {
			t.Fatalf("pixels changed at %v", p)
		}
	}
	path := filepath.Join(t.TempDir(), "nested", "result.png")
	if err = AtomicWrite(path, compressed, 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, compressed) {
		t.Fatal("atomic file incomplete")
	}
	for _, size := range []string{"100000x100000", "8192x8192", "-1x10", "nanx8"} {
		if _, _, err = Dimensions(size); err == nil {
			t.Fatalf("accepted %s", size)
		}
	}
}
