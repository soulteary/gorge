package filestorage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// UploadUsage is a bounded, read-only snapshot of logical file sizes. It does
// not return session IDs or paths and never deletes cancellation tombstones.
type UploadUsage struct {
	VolumeIdentity   string         `json:"volumeIdentity"`
	Entries          int            `json:"entries"`
	LogicalBytes     int64          `json:"logicalBytes"`
	Sessions         map[string]int `json:"sessions"`
	InvalidManifests int            `json:"invalidManifests"`
	Truncated        bool           `json:"truncated"`
	AvailableBytes   uint64         `json:"availableBytes"`
	NextCursor       string         `json:"nextCursor,omitempty"`
	Snapshot         string         `json:"snapshot"`
	Retention        string         `json:"retention"`
}

func (u *Uploads) Usage(ctx context.Context) (UploadUsage, error) {
	return u.UsagePage(ctx, "", 10000)
}

func (u *Uploads) UsagePage(ctx context.Context, cursor string, pageSize int) (UploadUsage, error) {
	if pageSize < 1 || pageSize > 10000 {
		return UploadUsage{}, errors.New("invalid page size")
	}
	if cursor != "" && len(cursor) != 64 {
		return UploadUsage{}, errors.New("invalid cursor")
	}
	found := cursor == ""
	last := ""
	out := UploadUsage{VolumeIdentity: u.volumeID, Sessions: map[string]int{}, Snapshot: "observational; quiesce writers for consistent multi-page totals", Retention: "partial=7d; complete=reference-owned; cancelled manifests/locks=indefinite"}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(u.root, &stat); err != nil {
		return out, err
	}
	out.AvailableBytes = stat.Bavail * uint64(stat.Bsize)
	limit := errors.New("inventory limit")
	err := filepath.WalkDir(u.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if path == u.root {
			return nil
		}
		rel, err := filepath.Rel(u.root, path)
		if err != nil {
			return err
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(rel)))
		if !found {
			if digest == cursor {
				found = true
			}
			return nil
		}
		if out.Entries >= pageSize {
			out.Truncated = true
			out.NextCursor = last
			return limit
		}
		last = digest
		out.Entries++
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out.LogicalBytes += info.Size()
		if entry.Name() == "manifest.json" && filepath.Dir(filepath.Dir(path)) == u.root {
			// Concurrent atomic replacement is harmless: this is an observational
			// snapshot, not an integrity check or a retirement gate.
			if info.Size() > 8<<20 {
				out.InvalidManifests++
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
			_ = f.Close()
			if len(raw) > 8<<20 {
				out.InvalidManifests++
				return nil
			}
			if err != nil {
				return err
			}
			var manifest Upload
			if json.Unmarshal(raw, &manifest) != nil {
				out.InvalidManifests++
				return nil
			}
			switch manifest.State {
			case "uploading", "complete", "cancelled":
				out.Sessions[manifest.State]++
			default:
				out.InvalidManifests++
			}
		}
		return nil
	})
	if errors.Is(err, limit) {
		err = nil
	}
	if err == nil && !found {
		return out, errors.New("cursor no longer exists; restart inventory")
	}
	return out, err
}
