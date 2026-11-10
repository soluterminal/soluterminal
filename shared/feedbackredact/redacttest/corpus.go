// Package redacttest runs the single synthetic corpus against either adapter.
// Only test files import this package; fixtures are not embedded in the app.
package redacttest

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
)

//go:embed testdata/corpus.json
var corpus []byte

type Case struct{ Name, Input, Want string }

func Cases(t testing.TB) []Case {
	t.Helper()
	var cases []Case
	if err := json.Unmarshal(corpus, &cases); err != nil {
		t.Fatal(err)
	}
	// Fixed alphabet repetitions are deliberately fake. Fragment expansion
	// prevents secret scanners from treating the corpus as stored credentials.
	fake := strings.Repeat("abcdefghijklmnop", 4)
	replacements := []string{"{{fake35}}", fake[:35], "{{fake24}}", fake[:24], "{{fake6}}", fake[:6], "{{fake27}}", fake[:27], "{{fake16upper}}", "ABCDEFGHIJKLMNOP"}
	// Encode PEM markers in the fixture file so scans never mistake fake
	// key examples for stored key material. Decoding preserves exact bytes.
	dash := strings.Repeat("-", 5)
	for _, kind := range []string{"RSA", "EC", "OPENSSH", "NONE"} {
		label := kind + " "
		if kind == "NONE" {
			label = ""
		}
		for _, edge := range []string{"BEGIN", "END"} {
			replacements = append(replacements, "{{pem_"+strings.ToLower(edge)+"_"+kind+"}}", dash+edge+" "+label+"PRIVATE KEY"+dash)
		}
	}
	expand := strings.NewReplacer(replacements...)
	for i := range cases {
		cases[i].Input = expand.Replace(cases[i].Input)
	}
	return cases
}

// Check asserts exact bytes as well as idempotence for every corpus entry.
func Check(t *testing.T, redact func(string) string) {
	t.Helper()
	for _, c := range Cases(t) {
		t.Run(c.Name, func(t *testing.T) {
			got := redact(c.Input)
			if got != c.Want {
				t.Fatalf("synthetic corpus mismatch: got %q; want %q", got, c.Want)
			}
			if again := redact(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}
