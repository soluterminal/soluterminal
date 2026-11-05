package main

import (
	"errors"
	"io"
	"os"
)

// The opt-in boundary is a finite snapshot, not a requirement that active
// transcript writers become idle. New bytes after the request wait for upload.
type usageSkipSnapshot struct {
	files []string
	sizes map[string]int64
}

func snapshotUsageSkips(files []string) (*usageSkipSnapshot, error) {
	s := &usageSkipSnapshot{sizes: make(map[string]int64, len(files))}
	for _, path := range files {
		fi, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("usage skip snapshot unavailable")
		}
		s.files = append(s.files, path)
		s.sizes[path] = fi.Size()
	}
	return s, nil
}

func usageSkipLimit(size int64, path string, snapshots []*usageSkipSnapshot) int64 {
	if len(snapshots) > 0 && snapshots[0] != nil {
		if limit := snapshots[0].sizes[path]; limit < size {
			return limit
		}
	}
	return size
}

func usageSkipWindow(path string, start, end int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, false
	}
	size := end - start
	if size > ccUsageMaxBytesPerFilePass {
		size = ccUsageMaxBytesPerFilePass
	}
	buf, err := io.ReadAll(io.LimitReader(f, size))
	return buf, err == nil
}

func (s *usageSkipSnapshot) reached(offset func(string) int64) bool {
	for _, path := range s.files {
		fi, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false
		}
		end := s.sizes[path]
		if fi.Size() < end {
			end = fi.Size()
		}
		if offset(path) < end {
			return false
		}
	}
	return true
}
