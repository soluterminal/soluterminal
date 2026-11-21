package helperproto_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	hp "soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"

	"github.com/hashicorp/yamux"
)

func TestReadHeaderKeepsTheKindOfAMistypedHeader(t *testing.T) {
	line := `{"s":"scan","op":"read","root":"codex-sessions","maxRecords":"SECRET_LIMIT","pane":"p1"}`
	h, err := hp.ReadHeader(strings.NewReader(line + "\n"))
	if err != nil {
		t.Fatalf("a mistyped header with a named kind was a stream error: %v", err)
	}
	if h.S != hp.KindScan || h.DecodeErr == nil {
		t.Fatal("kind or error lost")
	}
	// Nothing from the line but its kind reaches a handler: no Raw, no
	// half-decoded fields, and the error never quotes a value.
	if h.Raw != nil || strings.Contains(h.DecodeErr.Error(), "SECRET") {
		t.Fatal("a mistyped header kept or echoed the raw line")
	}
	if h.Op != "" || h.Root != "" || h.Pane != "" || h.MaxRecords != 0 {
		t.Fatal("a mistyped header kept partly decoded fields")
	}
	if err := hp.CheckScanHeader(h); hp.ErrorCode(err) != hp.CodeBadArgs {
		t.Fatal("a mistyped scan header passed the scan check")
	}
	// Re-encoding never carries the error or the raw line.
	var out bytes.Buffer
	if err := hp.WriteHeader(&out, h); err != nil || strings.Contains(out.String(), "SECRET") {
		t.Fatal("a mistyped header was echoed")
	}
}

// The error never carries a value from the line, whatever its type.
func TestDecodeErrNeverQuotesAValue(t *testing.T) {
	for _, line := range []string{
		`{"s":"scan","maxRecords":123456789012345678901234567890}`,
		`{"s":"scan","maxRecords":-1234567890123456789012}`,
		`{"s":"scan","maxRecords":"SECRET"}`,
		`{"s":"scan","files":[{"offset":"SECRET"}]}`,
		`{"s":"fwd","attest":1,"pane":["SECRET"]}`,
	} {
		h, err := hp.ReadHeader(strings.NewReader(line + "\n"))
		if err != nil || h.DecodeErr == nil {
			t.Fatalf("%s: %v", line, err)
		}
		msg := h.DecodeErr.Error()
		if strings.Contains(msg, "SECRET") || strings.Contains(msg, "1234567890") {
			t.Fatalf("the error quotes the line: %s", msg)
		}
	}
}

func TestReadHeaderStillRefusesUnnamedKinds(t *testing.T) {
	for name, line := range map[string]string{
		"bad-json":    `{"s":"scan",`,
		"no-kind":     `{"op":"read"}`,
		"empty-kind":  `{"s":"","op":5}`,
		"number-kind": `{"s":5}`,
		"null-kind":   `{"s":null,"op":5}`,
		"array":       `["scan"]`,
	} {
		t.Run(name, func(t *testing.T) {
			h, err := hp.ReadHeader(strings.NewReader(line + "\n"))
			if err == nil || h.DecodeErr != nil {
				t.Fatal("an unnamed or broken header was accepted")
			}
		})
	}
}

// A mistyped header reaches the registered handler with DecodeErr set, so the
// handler can answer bad_args; the session survives.
func TestMistypedHeaderReachesTheHostHandler(t *testing.T) {
	got := make(chan hp.StreamHeader, 1)
	p := helpertest.NewPair(t, helpertest.Options{
		Host: hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{
			hp.KindFwd: func(_ *hp.HostSession, h hp.StreamHeader, c net.Conn) {
				got <- h
				_ = c.Close()
			},
		}},
	})
	st, err := p.Helper.OpenRawForTest([]byte(`{"s":"fwd","kind":"http","pane":7}`))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	select {
	case h := <-got:
		if h.DecodeErr == nil || h.S != hp.KindFwd || h.Kind != "" || h.Pane != "" || h.Raw != nil {
			t.Fatal("the handler did not get the decode error alone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was not called")
	}
	stillUp(t, p)
}

func TestMistypedHeaderReachesTheHelperHandler(t *testing.T) {
	got := make(chan hp.StreamHeader, 1)
	p := helpertest.NewPair(t, helpertest.Options{
		Helper: hp.HelperOptions{Handlers: map[string]hp.HelperStreamHandler{
			hp.KindScan: func(_ *hp.HelperSession, h hp.StreamHeader, c net.Conn) {
				got <- h
				_ = c.Close()
			},
		}},
	})
	st, err := p.Host.OpenRawForTest([]byte(`{"s":"scan","op":"read","maxRecords":"many"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	select {
	case h := <-got:
		if h.DecodeErr == nil || hp.ErrorCode(hp.CheckScanHeader(h)) != hp.CodeBadArgs {
			t.Fatal("the scan handler could not tell the header was bad")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was not called")
	}
	stillUp(t, p)
}

// A mistyped agent or control header is still refused, as before: the
// helper closes the stream (EOF, not a timeout) and the session stays up.
func TestMistypedAgentAndControlHeadersAreRefused(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	for _, line := range []string{
		`{"s":"agent","agent":5}`,
		`{"s":"control","agent":5}`,
	} {
		st, err := p.Host.OpenRawForTest([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := st.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("%s: the stream was not closed by the helper: %v", line, err)
		}
		_ = st.Close()
	}
	stillUp(t, p)
}

// A mistyped first control stream ends the helper with a protocol break.
func TestMistypedFirstControlStreamIsABreak(t *testing.T) {
	hostEnd, helperEnd := helpertest.Pipe()
	helper, err := hp.NewHelperSession(helperEnd, helpertest.NewFakeHelper("v", 1000), hp.HelperOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serve := make(chan error, 1)
	go func() { serve <- helper.Serve() }()
	mux, err := yamux.Client(hostEnd, hp.YamuxConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close(); _ = hostEnd.Close(); _ = helper.Close() })
	ctl, err := mux.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Write([]byte(`{"s":"control","agent":5}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serve:
		if !hp.IsBreak(err) {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a mistyped control header did not end the helper")
	}
}

// On the host, a handler registered under a reserved kind never sees a
// mistyped header: that is still a protocol break.
func TestHostBreaksOnMistypedReservedKinds(t *testing.T) {
	for _, kind := range []string{hp.KindControl, hp.KindAgent} {
		t.Run(kind, func(t *testing.T) {
			called := make(chan struct{}, 1)
			p := helpertest.NewPair(t, helpertest.Options{
				Host: hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{
					kind: func(_ *hp.HostSession, _ hp.StreamHeader, c net.Conn) { called <- struct{}{}; _ = c.Close() },
				}},
			})
			if st, err := p.Helper.OpenRawForTest([]byte(`{"s":"` + kind + `","agent":5}`)); err == nil {
				defer st.Close()
			}
			if err := waitDone(t, p.Host); !hp.IsBreak(err) {
				t.Fatalf("host session: %v", err)
			}
			select {
			case <-called:
				t.Fatal("the reserved-kind handler saw a mistyped header")
			default:
			}
		})
	}
}

func stillUp(t *testing.T, p *helpertest.Pair) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Host.List(ctx, hp.ListArgs{}); err != nil {
		t.Fatalf("the session broke: %v", err)
	}
}
