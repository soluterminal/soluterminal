package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Since the Focus (native) panes were removed (ST-254), the phone one-tap that
// #1120/#1121 hardened has nothing to act on. These tests pin what @secure
// required of that removal: no path resolves an approval by digest, the daemon
// signs and offers no one-tap, and the widening classifier still guards any
// allowlist auto-allow.

// Since the Focus panes were removed, no route resolves an approval: the old
// one-tap endpoint is gone, so an old phone build's Approve with an
// approvalDigest reaches no handler and answers nothing (ST-193 strip).
func TestNoRouteResolvesAnApprovalDigest(t *testing.T) {
	mux := http.NewServeMux()
	(&apiServer{metrics: NewMetrics()}).routes(mux)
	body := `{"allow":true,"approvalDigest":"` + strings.Repeat("A", 43) + `"}`
	req := httptest.NewRequest("POST", "/api/panes/pane_x/approvals/r1/resolve", strings.NewReader(body))
	if _, pattern := mux.Handler(req); pattern != "" {
		t.Fatalf("POST %s is still routed to %q; an approvalDigest must reach no resolve handler", req.URL.Path, pattern)
	}
	// No surviving request type decodes an approvalDigest either, except
	// terminal one-tap's resolve (ST-265), which binds the digest to a live
	// signed entry for the prompt on screen (terminal_one_tap_resolve.go).
	for _, f := range daemonSourceFiles(t) {
		if f.name == "terminal_one_tap_resolve.go" {
			continue
		}
		if strings.Contains(f.text, `json:"approvalDigest`) {
			t.Errorf("%s decodes approvalDigest; nothing may resolve an approval by digest without a live signed entry", f.name)
		}
	}
}

// Nothing builds or sends an approval signature for the removed native
// approvals: the phone gets no requestId and no signature, so it offers
// Review (ST-193 strip). The one exception is terminal one-tap (ST-265):
// approval_signer.go signs, and pusher.go sends promptId with the signature
// only when every terminal one-tap check passes; a terminal menu without them
// gets nothing (asserted below).
func TestDaemonSendsNoApprovalSignatureOrOneTap(t *testing.T) {
	terminalOneTap := map[string]bool{"approval_signature.go": true, "approval_signer.go": true}
	// Needles are prefixes so a struct tag (`json:"widensPermissions,omitempty"`)
	// or a map key matches as well as a literal field.
	needles := []string{"approvalSignedMessage(", ".sign(msg", `"approvalSig`, `"approvalIssuedAt`, `"requestId`, `"widensPermissions`}
	// The only lines allowed to carry one, each exactly once: pusher.go sets
	// the signed fields of a terminal one-tap that passed every check (ST-265)
	// and strips them from a private-mode push; ccusage_extract.go reads
	// Claude's usage-log request id, which is not an approval. Lines are
	// matched whole (trimmed), so a signing call on the right-hand side, or a
	// copy in another push path, fails.
	allowed := map[string]map[string]int{
		"pusher.go": {
			`fields["widensPermissions"] = true`:                                  1,
			`fields["widensPermissions"] = false`:                                 1,
			`fields["approvalSig"] = signed.sig`:                                  1,
			`fields["approvalSigV"] = approvalSigVersion`:                         1,
			`fields["approvalIssuedAt"] = strconv.FormatInt(signed.issuedAt, 10)`: 1,
			`delete(m, "approvalSig")`:                                            1,
			`delete(m, "approvalSigV")`:                                           1,
			`delete(m, "approvalIssuedAt")`:                                       1,
		},
		"ccusage_extract.go": {"RequestID   string `json:\"requestId\"`": 1},
	}
	for _, f := range daemonSourceFiles(t) {
		if terminalOneTap[f.name] {
			continue
		}
		seen := map[string]int{}
		for i, line := range strings.Split(f.text, "\n") {
			trimmed := strings.TrimSpace(line)
			for _, needle := range needles {
				if !strings.Contains(line, needle) {
					continue
				}
				if max, ok := allowed[f.name][trimmed]; ok && seen[trimmed] < max {
					seen[trimmed]++
					break
				}
				t.Errorf("%s:%d contains %s; the daemon must not sign or offer a one-tap approval", f.name, i+1, needle)
			}
		}
	}

	// A terminal pane at a permission menu: its push and its pane-list entry
	// carry no one-tap handle.
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/notify" {
			b, _ := io.ReadAll(r.Body)
			bodies <- b
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	withTestApprovalSigner(t, "mach_strip")
	pusher := &Pusher{cloudBase: server.URL, token: "synthetic-credential", client: server.Client()}
	p := testPaneWithTail(t, "terminal-menu-push", "Fixture", "Do you want to proceed?\n❯ 1. Yes\n  2. No")
	pusher.fire(p, string(StateChoicePrompt))
	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(2 * time.Second):
		t.Fatal("push not delivered")
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"requestId", "approvalSig", "approvalSigV", "approvalIssuedAt", "widensPermissions"} {
		if _, ok := fields[k]; ok {
			t.Fatalf("push carries %q: %s", k, body)
		}
	}
	dto, err := json.Marshal(makePaneDTO(p, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"pendingApproval"`, `"approvalSig"`, `"requestId"`} {
		if strings.Contains(string(dto), k) {
			t.Fatalf("pane DTO carries %s: %s", k, dto)
		}
	}
}

type daemonSource struct{ name, text string }

// daemonSourceFiles returns the package's non-test Go sources.
func daemonSourceFiles(t *testing.T) []daemonSource {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []daemonSource
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, daemonSource{p, string(b)})
	}
	return out
}

// The widening classifier must run wherever an allowlist auto-allow exists.
// Since the Focus panes were removed there is none (the native tool/prefix
// allowlist went with them); this fails if one comes back without calling
// approvalWidening first (ST-192 strip).
func TestEveryAllowlistAutoAllowChecksWidening(t *testing.T) {
	autoAllow := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w*[Aa]llowlist\w*Allows?\w*)\(`)
	for _, f := range daemonSourceFiles(t) {
		for _, m := range autoAllow.FindAllStringSubmatchIndex(f.text, -1) {
			name := f.text[m[2]:m[3]]
			body := f.text[m[0]:]
			if end := strings.Index(body, "\n}\n"); end >= 0 {
				body = body[:end]
			}
			if !strings.Contains(body, "approvalWidening(") {
				t.Errorf("%s: %s auto-allows from an allowlist without calling approvalWidening", f.name, name)
			}
		}
	}
}
