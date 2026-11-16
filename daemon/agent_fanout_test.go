package main

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// burstChunks returns n distinct numbered chunks, like a fast `seq` arriving
// in PTY-read sized pieces.
func burstChunks(n, size int) [][]byte {
	chunks := make([][]byte, n)
	for i := range chunks {
		line := []byte(fmt.Sprintf("%06d lorem ipsum dolor sit amet consectetur\n", i))
		chunk := bytes.Repeat(line, max(1, size/len(line)))
		chunks[i] = chunk
	}
	return chunks
}

// writeBurst appends chunks the way the agent's PTY reader does: ring write
// and fan-out under the same lock.
func writeBurst(mu *sync.Mutex, ring *RingBuffer, fan *agentFanout, chunks [][]byte) []byte {
	var all []byte
	for _, c := range chunks {
		mu.Lock()
		_, _ = ring.Write(c)
		fan.fanoutLocked(c)
		mu.Unlock()
		all = append(all, c...)
	}
	return all
}

// collect drains ch until it holds want bytes, it closes, or the timeout hits.
func collect(t *testing.T, ch <-chan []byte, want int, timeout time.Duration) []byte {
	t.Helper()
	var got []byte
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case b, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, b...)
		case <-deadline:
			return got
		}
	}
	return got
}

// A daemon connection that is busy while a burst arrives must still receive
// every byte. The old 64-slot channel dropped chunks once it filled, which
// left holes in the middle of the daemon's stored history.
func TestAgentFanoutBurstToBusyConnectionLosesNothing(t *testing.T) {
	var mu sync.Mutex
	ring := NewRingBuffer(ringBufferBytes)
	fan := newAgentFanout(ring, false)
	conn := new(net.UnixConn)

	mu.Lock()
	ch := fan.addLocked(conn)
	mu.Unlock()

	// The connection reads nothing while 3,000 chunks arrive.
	all := writeBurst(&mu, ring, fan, burstChunks(3000, 512))

	got := collect(t, ch, len(all), 10*time.Second)
	if !bytes.Equal(got, all) {
		t.Fatalf("connection received %d of %d bytes; first difference at byte %d", len(got), len(all), firstDiff(got, all))
	}
}

// The stream for a new connection starts exactly where the replay snapshot
// taken under the same lock ends: no byte is sent twice or skipped.
func TestAgentFanoutStreamStartsAtSnapshotEnd(t *testing.T) {
	var mu sync.Mutex
	ring := NewRingBuffer(ringBufferBytes)
	fan := newAgentFanout(ring, false)

	before := writeBurst(&mu, ring, fan, burstChunks(10, 256))
	mu.Lock()
	snapshot := ring.Snapshot()
	ch := fan.addLocked(new(net.UnixConn))
	mu.Unlock()
	after := writeBurst(&mu, ring, fan, burstChunks(20, 256))

	if !bytes.Equal(snapshot, before) {
		t.Fatalf("snapshot = %d bytes, want %d", len(snapshot), len(before))
	}
	got := collect(t, ch, len(after), 5*time.Second)
	if !bytes.Equal(got, after) {
		t.Fatalf("stream = %d bytes, want exactly the %d bytes after the snapshot", len(got), len(after))
	}
}

// Removing a connection hands over what was already written, then closes the
// writer's channel.
func TestAgentFanoutRemoveFlushesThenCloses(t *testing.T) {
	var mu sync.Mutex
	ring := NewRingBuffer(ringBufferBytes)
	fan := newAgentFanout(ring, false)
	conn := new(net.UnixConn)

	mu.Lock()
	ch := fan.addLocked(conn)
	mu.Unlock()
	all := writeBurst(&mu, ring, fan, burstChunks(200, 512))
	mu.Lock()
	fan.removeLocked(conn)
	if fan.countLocked() != 0 || fan.hasLocked(conn) {
		t.Fatal("connection still registered after remove")
	}
	mu.Unlock()

	var got []byte
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				if !bytes.Equal(got, all) {
					t.Fatalf("got %d of %d bytes before close", len(got), len(all))
				}
				return
			}
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("channel not closed after remove (got %d of %d bytes)", len(got), len(all))
		}
	}
}

// The exit broadcast waits for pumps to hand over their last bytes.
func TestAgentFanoutDetachAllWaitsForPumps(t *testing.T) {
	var mu sync.Mutex
	ring := NewRingBuffer(ringBufferBytes)
	fan := newAgentFanout(ring, false)

	mu.Lock()
	ch := fan.addLocked(new(net.UnixConn))
	mu.Unlock()
	all := writeBurst(&mu, ring, fan, burstChunks(100, 512))

	var got []byte
	drained := make(chan struct{})
	go func() {
		for b := range ch {
			got = append(got, b...)
		}
		close(drained)
	}()
	mu.Lock()
	conns, wait := fan.detachAllLocked(2 * time.Second)
	mu.Unlock()
	if len(conns) != 1 {
		t.Fatalf("detached %d connections, want 1", len(conns))
	}
	wait()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("writer channel not closed after detach")
	}
	if !bytes.Equal(got, all) {
		t.Fatalf("got %d of %d bytes before exit", len(got), len(all))
	}
}

// A connection that falls more than a whole ring behind resumes at the oldest
// byte still held, in order, ending with the newest output.
func TestAgentFanoutConnectionBehindWholeRingResumesAtOldest(t *testing.T) {
	var mu sync.Mutex
	const ringSize = 64 * 1024
	ring := NewRingBuffer(ringSize)
	fan := newAgentFanout(ring, false)

	mu.Lock()
	ch := fan.addLocked(new(net.UnixConn))
	mu.Unlock()
	all := writeBurst(&mu, ring, fan, burstChunks(512, 512)) // 4x the ring

	// Wait until the stream reaches the newest byte.
	var got []byte
	var pieces [][]byte
	deadline := time.After(5 * time.Second)
	for !bytes.HasSuffix(got, all[len(all)-ringSize:]) {
		select {
		case b := <-ch:
			got = append(got, b...)
			pieces = append(pieces, b)
		case <-deadline:
			t.Fatalf("stream never reached the newest output (got %d bytes)", len(got))
		}
	}
	if len(got) > len(all) {
		t.Fatalf("got %d bytes, more than the %d written", len(got), len(all))
	}
	// A connection that fell behind skips what the ring overwrote, so the
	// stream may jump between pieces (even inside its first 512 bytes, when the
	// first read found one short write). Each piece is one read of the ring and
	// must be output that was written, in one piece, after the piece before it.
	// Searching from the previous piece's end finds a match no later than the
	// true one, so a correct stream always passes.
	pos := 0
	for i, piece := range pieces {
		at := bytes.Index(all[pos:], piece)
		if at < 0 {
			t.Fatalf("piece %d of %d (%d bytes) is not output written after the piece before it", i, len(pieces), len(piece))
		}
		pos += at + len(piece)
	}
}

// No-PTY connections keep a direct channel with blocking sends.
func TestAgentFanoutNoPtyUsesDirectChannel(t *testing.T) {
	var mu sync.Mutex
	ring := NewRingBuffer(ringBufferBytes)
	fan := newAgentFanout(ring, true)
	mu.Lock()
	ch := fan.addLocked(new(net.UnixConn))
	mu.Unlock()
	if len(fan.pumps) != 0 {
		t.Fatal("no-PTY connection got a ring pump")
	}
	go writeBurst(&mu, ring, fan, burstChunks(100, 256))
	got := collect(t, ch, 100*len(burstChunks(1, 256)[0]), 5*time.Second)
	if len(got) != 100*len(burstChunks(1, 256)[0]) {
		t.Fatalf("got %d bytes", len(got))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
