package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// cc-usage incremental-scan cursors.
//
// The transcript scanner (a later, separate file) reads each Claude Code
// JSONL transcript incrementally: every pass it reads only the bytes that
// were appended since the previous pass. To do that it remembers, per file,
// how far it has already consumed. That bookkeeping lives here.
//
// Design choice — PATH-keyed, not inode-keyed. The plan (A2) originally
// suggested keying on (device, inode), but cursors are keyed on the file's
// absolute PATH for cross-platform simplicity (Windows has no stable inode
// notion, and Go's os.FileInfo doesn't expose device/inode portably).
// Truncation / recreation is detected from the (size, mtime) we store
// alongside the offset rather than from inode identity:
//
//   - If the file's current size is smaller than our stored Offset, the file
//     was truncated (or replaced by a smaller one) — we restart from 0.
//   - If the file's current size is smaller than the size we last recorded,
//     or its mtime moved backwards, the file was replaced/rewound — restart
//     from 0.
//   - Otherwise the stored Offset is still valid and we resume from it.
//
// Persistence mirrors PaneStore exactly: load on startup, quarantine the
// file and start fresh on corrupt JSON, and save atomically via
// writeFileAtomicSynced.

// scanCursor is the per-file persisted position. Path is the map key, so it
// is not stored inside the value.
type scanCursor struct {
	// Offset is the byte offset just past the last fully-consumed line.
	Offset int64 `json:"offset"`
	// Size is the file size (bytes) observed when Offset was recorded. Used
	// to detect replacement/rewind on a later pass.
	Size int64 `json:"size"`
	// ModTimeUnixNano is the file mtime (nanoseconds since the Unix epoch)
	// observed when Offset was recorded. Used to detect replacement.
	ModTimeUnixNano int64 `json:"modTimeUnixNano"`
	// LastBilledTurnTS is the latest valid billed-turn timestamp committed
	// atomically with Offset. Legacy cursor files omit it.
	LastBilledTurnTS string `json:"lastBilledTurnTs,omitempty"`
	// LastBilledMessageID and LastBilledRequestID identify that turn. Claude
	// may append growing streamed copies of one billed turn; retaining its
	// identity prevents those copies from becoming false inter-turn gaps.
	LastBilledMessageID string `json:"lastBilledMessageId,omitempty"`
	LastBilledRequestID string `json:"lastBilledRequestId,omitempty"`
	// WorkflowAttributionKey records the workflow metadata applied when the
	// consumed rows were last acknowledged. If an observer supplies stronger
	// metadata later, the scanner can safely replay the transcript for cloud
	// keep-max enrichment even when no new bytes were appended.
	WorkflowAttributionKey string `json:"workflowAttributionKey,omitempty"`
}

// cursorStore is the thread-safe, persisted collection of per-file scan
// cursors. The scanner goroutine and (potentially) other callers touch it
// concurrently, so every method takes the mutex.
type cursorStore struct {
	path          string
	mu            sync.Mutex
	m             map[string]scanCursor
	checkpoint    usageCheckpoint[scanCursor]
	uploadAllowed func() bool
}

// newCursorStore constructs an empty store backed by
// <dataDir>/cc-usage-cursors.json. It does NOT read from disk — call Load
// once at startup.
func newCursorStore(dataDir string) *cursorStore {
	return &cursorStore{
		path:       filepath.Join(dataDir, "cc-usage-cursors.json"),
		m:          make(map[string]scanCursor),
		checkpoint: usageCheckpoint[scanCursor]{lastSave: time.Now()},
	}
}

// Load reads the persisted cursor map. A missing file is not an error (the
// store stays empty). A corrupt file is quarantined aside and the store
// starts fresh, so a torn write can never wedge the scanner.
func (s *cursorStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var m map[string]scanCursor
	if err := json.Unmarshal(b, &m); err != nil {
		// Corrupt/torn cursor file: quarantine for forensics and start
		// fresh. Re-scanning from offset 0 is safe — the cloud upsert is
		// idempotent (keep-max), so a full re-read just re-sends rows the
		// DB already has.
		quarantineCorruptState(s.path, err)
		s.m = make(map[string]scanCursor)
		return nil
	}
	if m == nil {
		m = make(map[string]scanCursor)
	}
	s.m = m
	s.checkpoint.lastSave = s.checkpoint.clock()
	return nil
}

// Get returns the stored byte offset for path. ok is false if the file has
// never been scanned. Callers that need replacement detection should prefer
// ShouldResetTo, which folds in the current stat.
func (s *cursorStore) Get(path string) (offset int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[path]
	return c.Offset, ok
}

// ShouldResetTo returns the byte offset the scanner should begin reading
// from for path, given the file's CURRENT size and mtime (as observed by a
// fresh os.Stat this pass). It returns:
//
//   - 0 if the file is new (never scanned), was truncated (curSize < stored
//     Offset), or was replaced/rewound (curSize < stored Size, or curModNano
//     moved backwards relative to the stored mtime); and
//   - the stored Offset otherwise (resume in place).
//
// This is the single source of truth for "where do I start reading?" — the
// scanner should call this rather than Get so truncation/replacement is
// handled uniformly.
func (s *cursorStore) ShouldResetTo(path string, curSize int64, curModNano int64) int64 {
	offset, _, _ := s.ReadState(path, curSize, curModNano)
	return offset
}

// ReadState returns the reset-aware offset and prior billed-turn timestamp.
// On truncation/replacement both are reset together.
func (s *cursorStore) ReadState(path string, curSize int64, curModNano int64) (offset int64, lastBilledTurnTS string, reset bool) {
	offset, lastBilledTurnTS, _, _, _, reset = s.ReadBilledState(path, curSize, curModNano)
	return
}

// ReadBilledState returns the reset-aware offset together with the last
// distinct billed-turn identity and the workflow attribution already applied
// to acknowledged rows. On truncation/replacement all fields reset together.
func (s *cursorStore) ReadBilledState(path string, curSize int64, curModNano int64) (offset int64, lastBilledTurnTS, lastBilledMessageID, lastBilledRequestID, workflowAttributionKey string, reset bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[path]
	if !ok {
		// Never seen — read from the start.
		return 0, "", "", "", "", false
	}
	if curSize < c.Offset {
		// Truncated below where we'd resume; the bytes we consumed are gone.
		return 0, "", "", "", "", true
	}
	if curSize < c.Size {
		// Smaller than last time without dropping below Offset still means
		// the file shrank — treat as a replacement and re-read.
		return 0, "", "", "", "", true
	}
	if curModNano < c.ModTimeUnixNano {
		// mtime moved backwards — the file was replaced by an older copy.
		return 0, "", "", "", "", true
	}
	return c.Offset, c.LastBilledTurnTS, c.LastBilledMessageID, c.LastBilledRequestID, c.WorkflowAttributionKey, false
}

// Set records a new cursor for path. offset is the byte position just past
// the last fully-consumed line; size and modNano are the file's stat at the
// moment that offset was reached. This only mutates memory — call Save to
// persist.
func (s *cursorStore) Set(path string, offset, size, modNano int64) {
	s.SetState(path, offset, size, modNano, "")
}

func (s *cursorStore) SetState(path string, offset, size, modNano int64, lastBilledTurnTS string) {
	s.SetBilledState(path, offset, size, modNano, lastBilledTurnTS, "", "", "")
}

func (s *cursorStore) SetBilledState(path string, offset, size, modNano int64, lastBilledTurnTS, lastBilledMessageID, lastBilledRequestID, workflowAttributionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.set(s.m, path, scanCursor{
		Offset: offset, Size: size, ModTimeUnixNano: modNano,
		LastBilledTurnTS: lastBilledTurnTS, LastBilledMessageID: lastBilledMessageID,
		LastBilledRequestID: lastBilledRequestID, WorkflowAttributionKey: workflowAttributionKey,
	})
}

// Save writes the cursor map to disk atomically (tempfile + fsync + rename),
// the same durability path PaneStore uses, so a crash mid-write can never
// leave a torn cursors.json.
func (s *cursorStore) Save() error { return s.save(true) }

func (s *cursorStore) save(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.save(s.path, s.m, force)
}

func (s *cursorStore) commitSkipped(updates map[string]scanCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.skip(s.m, updates)
	return s.checkpoint.save(s.path, s.m, true)
}

func (s *cursorStore) flushSkipped() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.checkpoint.pending) == 0 {
		return nil
	}
	return s.checkpoint.save(s.path, s.m, true)
}

func (s *cursorStore) revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.revision
}

func (s *cursorStore) nextCheckpoint() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.nextDelay()
}
