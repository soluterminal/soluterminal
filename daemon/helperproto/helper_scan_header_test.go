package helperproto_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	hp "soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"
)

func scanHeaderRef() hp.ScanFileRef {
	return hp.ScanFileRef{
		FileID: strings.Repeat("a", 64), Gen: strings.Repeat("b", 64), Offset: (8 << 20) + 123, Tail: strings.Repeat("d", 64), Skipping: true,
		Context: &hp.ScanContext{ThreadID: "thread", SessionID: "session", Model: "gpt-test-fast", CLIVersion: "0.150.0", ServiceTier: "priority", Subagent: true, HasRecords: true, InApp: true},
	}
}

func TestM4ScanHeaderRoundTrip(t *testing.T) {
	for _, want := range []hp.StreamHeader{
		{S: hp.KindScan, Op: "list", Root: "claude-projects", After: strings.Repeat("c", 64), Since: "2026-11-20T00:00:00.000000000Z", MaxFiles: 1000},
		{S: hp.KindScan, Op: "read", Root: "codex-sessions", Files: []hp.ScanFileRef{scanHeaderRef()}, MaxRecords: 5000},
		{S: hp.KindScan, Op: "status", Root: "credential-status"},
	} {
		t.Run(want.Op, func(t *testing.T) {
			if err := hp.CheckScanHeader(want); err != nil {
				t.Fatal(err)
			}
			seen := make(chan hp.StreamHeader, 1)
			p := helpertest.NewPair(t, helpertest.Options{
				Host: hp.HostOptions{OpenKinds: []string{hp.KindScan}},
				Helper: hp.HelperOptions{Handlers: map[string]hp.HelperStreamHandler{
					hp.KindScan: func(_ *hp.HelperSession, h hp.StreamHeader, c net.Conn) {
						defer c.Close()
						seen <- h
						_, _ = c.Write([]byte("fixture\n"))
					},
				}},
			})
			st, err := p.Host.Open(want)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
			if b, err := io.ReadAll(st); err != nil || string(b) != "fixture\n" {
				t.Fatal("fake scan did not finish")
			}
			select {
			case got := <-seen:
				want.Raw, _ = json.Marshal(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("header changed: %+v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not receive header")
			}
		})
	}
}

func TestM4ScanHeaderStaysDefaultOffAndNeedsHello(t *testing.T) {
	h := hp.StreamHeader{S: hp.KindScan, Op: "status", Root: "credential-status"}
	p := helpertest.NewPair(t, helpertest.Options{})
	if _, err := p.Host.Open(h); hp.ErrorCode(err) != hp.CodeUnsupported {
		t.Fatal("default host enabled scan")
	}
	q := helpertest.NewPair(t, helpertest.Options{Host: hp.HostOptions{OpenKinds: []string{hp.KindScan}}})
	st, err := q.Host.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, io.EOF) || q.Host.Err() != nil {
		t.Fatal("unregistered helper scan did not refuse cleanly")
	}
	r := helpertest.NewPair(t, helpertest.Options{NoHello: true, Host: hp.HostOptions{OpenKinds: []string{hp.KindScan}}})
	if _, err := r.Host.Open(h); err == nil {
		t.Fatal("scan opened before hello")
	}
}

func TestM4ScanHeaderOmitsUnusedFields(t *testing.T) {
	for _, h := range []hp.StreamHeader{{S: hp.KindControl}, {S: hp.KindAgent, Agent: strings.Repeat("a", 32)}, {S: hp.KindScan, Op: "status", Root: "credential-status"}} {
		b, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"after"`, `"maxFiles"`, `"files"`, `"maxRecords"`} {
			if bytes.Contains(b, []byte(key)) {
				t.Fatal("unused M4 field appeared on wire")
			}
		}
		if h.S != hp.KindScan && bytes.Contains(b, []byte(`"op"`)) {
			t.Fatal("M4 op appeared on existing stream")
		}
	}
}

func TestM4ScanHeaderBoundsWholeEncodedLine(t *testing.T) {
	ref := scanHeaderRef()
	ref.Context = &hp.ScanContext{ThreadID: strings.Repeat("t", 512), SessionID: strings.Repeat("s", 512), Model: strings.Repeat("m", 256), CLIVersion: strings.Repeat("v", 64), ServiceTier: strings.Repeat("p", 32)}
	h := hp.StreamHeader{S: hp.KindScan, Op: "read", Root: "codex-sessions", MaxRecords: 5000}
	for i := 0; i < hp.ScanReadFilesMax; i++ {
		ref.FileID = fmt.Sprintf("%064x", i)
		h.Files = append(h.Files, ref)
	}
	if err := hp.CheckScanHeader(h); !errors.Is(err, hp.ErrLineTooLong) {
		t.Fatal("aggregate header size was not bounded")
	}
	var wire bytes.Buffer
	if err := hp.WriteHeader(&wire, h); !errors.Is(err, hp.ErrLineTooLong) || wire.Len() != 0 {
		t.Fatal("oversized header was partially written")
	}
	h.Files = h.Files[:8]
	if err := hp.CheckScanHeader(h); err != nil {
		t.Fatal("valid smaller batch rejected")
	}
	if err := hp.WriteHeader(&wire, h); err != nil {
		t.Fatal(err)
	}
	got, err := hp.ReadHeader(&wire)
	h.Raw, _ = json.Marshal(h)
	if err != nil || !reflect.DeepEqual(got, h) {
		t.Fatal("bounded header lost checkpoint state")
	}
}

func TestM4ScanHeaderRejectsInvalidMetadata(t *testing.T) {
	base := hp.StreamHeader{S: hp.KindScan, Op: "read", Root: "codex-sessions", Files: []hp.ScanFileRef{scanHeaderRef()}}
	for name, mutate := range map[string]func(*hp.StreamHeader){
		"file-count": func(h *hp.StreamHeader) {
			h.Files = nil
			for i := 0; i <= hp.ScanReadFilesMax; i++ {
				f := scanHeaderRef()
				f.FileID = fmt.Sprintf("%064x", i)
				h.Files = append(h.Files, f)
			}
		},
		"duplicate-files": func(h *hp.StreamHeader) {
			f := scanHeaderRef()
			f.Offset++
			h.Files = append(h.Files, f)
		},
		"bad-id":                func(h *hp.StreamHeader) { h.Files[0].FileID = "SECRET_PATH" },
		"nonempty-initial-tail": func(h *hp.StreamHeader) { h.Files[0].Offset = 0 },
		"bad-tail":              func(h *hp.StreamHeader) { h.Files[0].Tail = "SECRET_TAIL" },
		"unsorted-files": func(h *hp.StreamHeader) {
			f := scanHeaderRef()
			f.FileID = strings.Repeat("0", 64)
			h.Files = append(h.Files, f)
		},
		"bidi-context":                func(h *hp.StreamHeader) { h.Files[0].Context.Model = "model\u202E" },
		"zero-width-context":          func(h *hp.StreamHeader) { h.Files[0].Context.Model = "model\u200B" },
		"line-separator-context":      func(h *hp.StreamHeader) { h.Files[0].Context.Model = "model\u2028" },
		"paragraph-separator-context": func(h *hp.StreamHeader) { h.Files[0].Context.Model = "model\u2029" },
		"negative-offset":             func(h *hp.StreamHeader) { h.Files[0].Offset = -1 },
		"unsafe-integer":              func(h *hp.StreamHeader) { h.Files[0].Offset = hp.ScanIntegerMax + 1 },
		"bad-context":                 func(h *hp.StreamHeader) { h.Files[0].Context.Model = "SECRET_MODEL\n" },
		"large-context": func(h *hp.StreamHeader) {
			h.Files[0].Context.ThreadID = strings.Repeat("<", 512)
			h.Files[0].Context.SessionID = strings.Repeat("<", 512)
		},
		"bad-limit":           func(h *hp.StreamHeader) { h.MaxRecords = 5001 },
		"wrong-root":          func(h *hp.StreamHeader) { h.Root = "SECRET_ROOT" },
		"foreign-kind-fields": func(h *hp.StreamHeader) { h.Agent = "SECRET_AGENT" },
	} {
		t.Run(name, func(t *testing.T) {
			h := base
			h.Files = []hp.ScanFileRef{scanHeaderRef()}
			mutate(&h)
			err := hp.CheckScanHeader(h)
			if hp.ErrorCode(err) != hp.CodeBadArgs || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid scan metadata accepted or echoed")
			}
		})
	}
	initial := scanHeaderRef()
	initial.Offset, initial.Tail, initial.Skipping, initial.Context = 0, "", false, nil
	if err := hp.CheckScanHeader(hp.StreamHeader{S: hp.KindScan, Op: "read", Root: "codex-sessions", Files: []hp.ScanFileRef{initial}}); err != nil {
		t.Fatal("initial file checkpoint rejected")
	}
	if err := hp.CheckScanHeader(hp.StreamHeader{S: hp.KindScan, Op: "status", Root: "credential-status", Since: "2026-11-20T00:00:00.000000000Z"}); hp.ErrorCode(err) != hp.CodeBadArgs {
		t.Fatal("status accepted another field")
	}
}

func TestM4ScanHeaderStreamCapAndFirstClose(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	p := helpertest.NewPair(t, helpertest.Options{
		Host:   hp.HostOptions{OpenKinds: []string{hp.KindScan}},
		Helper: hp.HelperOptions{Handlers: map[string]hp.HelperStreamHandler{hp.KindScan: func(_ *hp.HelperSession, _ hp.StreamHeader, c net.Conn) { defer c.Close(); <-hold }}},
	})
	h := hp.StreamHeader{S: hp.KindScan, Op: "status", Root: "credential-status"}
	var streams []net.Conn
	defer func() {
		for _, c := range streams {
			_ = c.Close()
		}
	}()
	for i := 0; i < hp.MaxStreams-1; i++ {
		c, err := p.Host.Open(h)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, c)
	}
	if _, err := p.Host.Open(h); !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatal("scan bypassed session stream cap")
	}
	_ = streams[0].Close()
	_ = streams[0].Close()
	c, err := p.Host.Open(h)
	if err != nil {
		t.Fatal("first Close did not free one slot")
	}
	streams = append(streams, c)
	if _, err := p.Host.Open(h); !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatal("second Close freed a second slot")
	}
}
