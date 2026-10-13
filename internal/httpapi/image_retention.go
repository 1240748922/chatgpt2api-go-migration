package httpapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) imageRetentionScheduler() {
	cleanup := func() {
		removed, size := s.cleanupExpiredImages()
		if removed > 0 {
			s.runtimeLog("info", "image retention removed %d files (%d bytes)", removed, size)
		}
	}
	cleanup()
	interval := s.cfg.ImageCleanupInterval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}
func (s *Server) cleanupExpiredImages() (int, int64) {
	days := s.cfg.ImageRetentionDays
	if s.store != nil {
		cfg, err := s.store.Config()
		if err != nil {
			s.runtimeLog("warning", "image retention: configuration unavailable")
			return 0, 0
		}
		if n := intValue(cfg["image_retention_days"]); n > 0 {
			days = n
		}
	}
	if days < 1 {
		days = 15
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if s.store != nil {
		lease, err := s.store.AcquireLease(ctx, "image-storage-maintenance", 30*time.Second)
		if err != nil || lease == nil {
			return 0, 0
		}
		defer lease.Release()
		lease.KeepAlive(ctx, 30*time.Second, cancel)
	}
	removed, size, err := s.cleanupImageFiles(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour), false)
	if err != nil {
		s.runtimeLog("warning", "image retention: %v", err)
	}
	return removed, size
}
func (s *Server) cleanupImageFiles(ctx context.Context, cutoff time.Time, dryRun bool) (int, int64, error) {
	count := 0
	var size int64
	err := filepath.Walk(s.cfg.ImageDataDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.IsDir() {
			if path != s.cfg.ImageDataDir && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || !isImageStorageFile(info.Name()) || !info.ModTime().Before(cutoff) {
			return nil
		}
		rel, err := filepath.Rel(s.cfg.ImageDataDir, path)
		if err != nil {
			return err
		}
		parts := []string{path, path + ".meta.json", s.thumbnailPath(path)}
		n := 0
		var bytes int64
		for _, part := range parts {
			if f, e := os.Lstat(part); e == nil && f.Mode().IsRegular() {
				n++
				bytes += f.Size()
			}
		}
		if !dryRun && !s.removeMediaPath(filepath.ToSlash(rel)) {
			return nil
		}
		count += n
		size += bytes
		return nil
	})
	if err == nil && s.store != nil {
		remote, e := s.remoteImageItems()
		if e != nil {
			return count, size, e
		}
		for _, item := range remote {
			if ctx.Err() != nil {
				return count, size, ctx.Err()
			}
			at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(item["updated_at"]))
			if at.IsZero() || !at.Before(cutoff) {
				continue
			}
			if dryRun || s.removeMediaPath(stringValue(item["path"])) {
				count++
				size += int64(intValue(item["size"]))
			}
		}
	}
	return count, size, err
}
