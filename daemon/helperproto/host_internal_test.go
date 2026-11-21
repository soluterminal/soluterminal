package helperproto

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// deliveredButShutdown writes the bytes, then reports what yamux reports
// when the peer closes the session before the write call returns.
type deliveredButShutdown struct{ w io.Writer }

func (d deliveredButShutdown) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, yamux.ErrSessionShutdown
}

type pipeEnd struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeEnd) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeEnd) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeEnd) Close() error                { _ = p.w.Close(); return p.r.Close() }

// rawPair returns a host session whose control writes "fail" after being
// delivered, and the helper's control stream.
func rawPair(t *testing.T) (*HostSession, *yamux.Session, *bufio.Reader, io.Writer) {
	t.Helper()
	a, b := io.Pipe()
	c, d := io.Pipe()
	hostEnd, helperEnd := &pipeEnd{r: c, w: b}, &pipeEnd{r: a, w: d}
	mux, err := yamux.Server(helperEnd, YamuxConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewHostSession(hostEnd, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(); _ = mux.Close() })
	ctl, err := mux.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHeader(ctl); err != nil {
		t.Fatal(err)
	}
	host.w = newLineWriter(deliveredButShutdown{w: host.ctl})
	return host, mux, bufio.NewReader(ctl), ctl
}

// The reply to a request yamux reported as unsent still counts (the flake
// in TestUnsafeRuntimeDirEndsTheHelper: "hello: session shutdown").
func TestReplyCountsAfterAWriteReportedShutdown(t *testing.T) {
	host, _, lines, ctl := rawPair(t)
	go func() {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return
		}
		var req Request
		_ = json.Unmarshal(line, &req)
		res, _ := json.Marshal(HelloResult{Proto: 1, HelperVersion: "v", UID: 1000, RuntimeDir: "/r", Networking: NetworkingNAT})
		b, _ := json.Marshal(Reply{ID: req.ID, OK: true, Result: res})
		_, _ = ctl.Write(append(b, '\n'))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := host.Hello(ctx, HelloArgs{Proto: 1, HostVersion: "v"}, 1000); err != nil {
		t.Fatalf("hello: %v", err)
	}
}

// With no reply, the session's end is the answer: no hang.
func TestNoReplyAfterAWriteReportedShutdownEnds(t *testing.T) {
	host, mux, lines, _ := rawPair(t)
	go func() {
		_, _ = lines.ReadBytes('\n')
		_ = mux.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := host.Hello(ctx, HelloArgs{Proto: 1, HostVersion: "v"}, 1000); err == nil || ctx.Err() != nil {
		t.Fatalf("hello: %v (ctx %v)", err, ctx.Err())
	}
}
