package provider

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
)

func TestRecoveredImageDeliveryPreservesFileAndRetentionAge(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "delivery.png")
	if err := writeMediaFile(root, "delivery", ".png", []byte("original-result")); err != nil {
		t.Fatal(err)
	}
	baseline := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, baseline, baseline); err != nil {
		t.Fatal(err)
	}
	if err := writeMediaFile(root, "delivery", ".png", []byte("original-result")); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if !info.ModTime().Equal(baseline) {
		t.Fatal("recovery extended retention age")
	}
	if err := writeMediaFile(root, "delivery", ".png", []byte("different-result")); err == nil {
		t.Fatal("delivery overwrote a previously returned image")
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != "original-result" {
		t.Fatal("original delivery changed")
	}
}

func TestConcurrentDeliveryCollisionAndCompressedReplay(t *testing.T) {
	root := t.TempDir()
	values := [][]byte{[]byte("first-image"), []byte("second-image")}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := range values {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if writeMediaFile(root, "shared", ".png", values[i]) == nil {
				results <- i
			}
		}(i)
	}
	wg.Wait()
	close(results)
	winners := []int{}
	for i := range results {
		winners = append(winners, i)
	}
	if len(winners) != 1 {
		t.Fatalf("collision winners=%v", winners)
	}
	actual, err := os.ReadFile(filepath.Join(root, "shared.png"))
	if err != nil || !bytes.Equal(actual, values[winners[0]]) {
		t.Fatal("collision changed saved image")
	}
	im := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	im.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	compressed, err := mediafile.RecompressPNG(context.Background(), buf.Bytes())
	if err != nil || len(compressed) >= buf.Len() {
		t.Fatal("PNG was not compressed")
	}
	path := filepath.Join(root, "compressed.png")
	if err = writeMediaFile(root, "compressed", ".png", compressed); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err = os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
	if err = writeMediaFile(root, "compressed", ".png", buf.Bytes()); err != nil {
		t.Fatalf("lossless replay failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(before) {
		t.Fatal("replay changed retention age")
	}
}
