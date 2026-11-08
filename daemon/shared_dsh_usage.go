package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// Each workflow invocation owns its session root. Never subtract global usage
// counters: concurrent tasks/accounts would contaminate the result. Multiple
// sessions can contain inherited fork histories, so leave those unknown until
// the harness exposes per-request identities that allow safe deduplication.
//
// A panic while decoding or reading the log (a corrupt or crafted file, as
// GO-2026-6865 could cause in the zstd decoder) leaves the usage unknown
// instead of taking the daemon down.
func readDSHInvocationUsage(root string) (input, output *int64) {
	defer func() {
		if recover() != nil {
			input, output = nil, nil
		}
	}()
	files, err := filepath.Glob(filepath.Join(root, "*", "*", "session*.jsonl.zstd"))
	if err != nil || len(files) != 1 {
		return nil, nil
	}
	f, err := os.Open(files[0])
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	decoder, err := zstd.NewReader(f, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, nil
	}
	defer decoder.Close()
	limited := &io.LimitedReader{R: decoder, N: 64 << 20}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var in, out int64
	count := 0
	completed := false
	for scanner.Scan() {
		usageScanLine(scanner.Bytes())
		var event dshUsageEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return nil, nil
		}
		switch event.Type {
		case "turn/start":
			completed = false
		case "turn/end":
			var payload struct {
				Reason struct {
					Kind string `json:"kind"`
				} `json:"reason"`
			}
			if json.Unmarshal(event.Data, &payload) != nil {
				return nil, nil
			}
			completed = payload.Reason.Kind == "completed"
		case "assistant/message":
			var payload struct {
				Usage *struct {
					Input  *int64 `json:"inputTokens"`
					Output *int64 `json:"outputTokens"`
					Read   int64  `json:"cacheReadTokens"`
					Write  int64  `json:"cacheWriteTokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Data, &payload) != nil || payload.Usage == nil || payload.Usage.Input == nil || payload.Usage.Output == nil {
				return nil, nil
			}
			u := payload.Usage
			if *u.Input < 0 || *u.Output < 0 || u.Read < 0 || u.Write < 0 || *u.Input > 1e12 || *u.Output > 1e12 || u.Read > 1e12 || u.Write > 1e12 || count > 100000 {
				return nil, nil
			}
			in += *u.Input + u.Read + u.Write
			out += *u.Output
			count++
		}
	}
	if scanner.Err() != nil || limited.N == 0 || count == 0 || !completed {
		return nil, nil
	}
	return &in, &out
}
