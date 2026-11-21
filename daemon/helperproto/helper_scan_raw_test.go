package helperproto_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	hp "soluterminal/helperproto"
)

func TestReadHeaderPreservesReceivedLine(t *testing.T) {
	for _, kind := range []string{hp.KindScan, hp.KindAgent, hp.KindFwd, hp.KindControl} {
		t.Run(kind, func(t *testing.T) {
			// Keep whitespace, key order and unknown fields, including an
			// attempted Raw assignment. Generic decoding remains additive.
			line := []byte(` { "future":true, "Raw":"UNTRUSTED", "s":"` + kind + `" } `)
			wire := append(append(append([]byte(nil), line...), '\n'), []byte("PAYLOAD\x00\n")...)
			r := bytes.NewReader(wire)
			h, err := hp.ReadHeader(r)
			if err != nil || h.S != kind || !bytes.Equal(h.Raw, line) {
				t.Fatal("received header line was changed or not retained")
			}
			payload, err := io.ReadAll(r)
			if err != nil || string(payload) != "PAYLOAD\x00\n" {
				t.Fatal("header read consumed stream payload")
			}
			// A received header can be written again, but raw input is never
			// serialized as another field or echoed as an unknown extension.
			var out bytes.Buffer
			if err := hp.WriteHeader(&out, h); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 1 || fields["s"] == nil || bytes.Contains(out.Bytes(), []byte("UNTRUSTED")) {
				t.Fatal("raw input was serialized or echoed")
			}
		})
	}
}

func TestReadHeaderRawRespectsLineBound(t *testing.T) {
	base := `{"s":"scan","future":true}`
	line := base + strings.Repeat(" ", hp.MaxLine-len(base))
	h, err := hp.ReadHeader(strings.NewReader(line + "\n"))
	if err != nil || len(h.Raw) != hp.MaxLine || string(h.Raw) != line {
		t.Fatal("exactly-MaxLine header was rejected or changed")
	}
	h, err = hp.ReadHeader(strings.NewReader(line + " \n"))
	if !errors.Is(err, hp.ErrLineTooLong) || h.Raw != nil {
		t.Fatal("oversized header was retained or accepted")
	}
	h, err = hp.ReadHeader(strings.NewReader(`{"s":"scan",` + "\n"))
	if err == nil || h.Raw != nil {
		t.Fatal("failed JSON decoding retained raw bytes")
	}
}
