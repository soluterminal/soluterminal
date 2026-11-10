package redacttest

import (
	"strings"
	"testing"
)

func TestPEMFixturesDecodeRealMarkers(t *testing.T) {
	required := map[string]bool{"private key RSA": false, "private key EC": false, "private key OPENSSH": false, "private key ": false}
	for _, c := range Cases(t) {
		if _, ok := required[c.Name]; !ok {
			continue
		}
		if !strings.Contains(c.Input, "-----BEGIN") || !strings.Contains(c.Input, "PRIVATE KEY-----") || !strings.Contains(c.Input, "-----END") {
			t.Fatal("escaped synthetic PEM fixture must decode its real private-key markers")
		}
		required[c.Name] = true
	}
	for name, found := range required {
		if !found {
			t.Fatalf("required synthetic PEM fixture missing: %s", name)
		}
	}
}
