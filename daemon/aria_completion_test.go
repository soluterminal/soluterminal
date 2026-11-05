package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests never start an installed daemon, provider, or CLI. Every file
// belongs to a synthetic HOME and every completion goes to this HTTP fixture.
func completionTestVerdict(t *testing.T, enabled bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOLU_TEAM_FEATURES", "")
	t.Setenv("SOLU_OPERATOR_FEATURES", "")
	previous := jevEntitlementsRef.Load()
	t.Cleanup(func() { jevEntitlementsRef.Store(previous) })
	ent := newTeamEntitlements(t.TempDir(), nil)
	ent.store(entitlementRecord{Team: enabled, Operator: &enabled, Tier: tierTeam, CheckedAt: time.Now()})
	jevEntitlementsRef.Store(ent)
}

func completionTestServer(t *testing.T) (*Aria, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aria/complete" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`+"\n\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"summary\":\"Review the request\",\"risk\":\"low\"}"}}`+"\n\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`+"\n\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	p := testPaneWithTail(t, "completion-pane", "Fixture", "Waiting for review\n")
	a := testAriaWithPane(t, p)
	a.settings = &ariaSettingsStore{}
	a.setCloudCreds(server.URL, "synthetic-credential")
	return a, &calls
}

func completionPayload(t *testing.T, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal("could not encode synthetic request")
	}
	return raw
}

func TestCompletionBoundaryFields(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	for _, field := range []string{"title", "reason", "observation", "instruction", "model", "future"} {
		for _, kind := range []string{"safe", "keyed", "opaque", "wrapped", "escaped"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				value := "Please review this change."
				switch kind {
				case "keyed":
					value = "api_key=fixture-value"
				case "opaque":
					value = "Q7n2V8m4L9r3X6k1T5c0B2d8W4f6Z9p3"
				case "wrapped":
					value = "Bearer abcdefgh\nijklmnop\nqrstuvwxyz"
				case "escaped":
					value = "password=fixture"
				}
				body := map[string]any{"messages": []ariaMessage{{Role: "user", Content: []ariaBlock{{Type: "text", Text: "Review"}}}}, field: map[string]any{"nested": value}}
				payload := completionPayload(t, body)
				if kind == "escaped" {
					payload = []byte(strings.ReplaceAll(string(payload), "password", `\u0070assword`))
				}
				before := calls.Load()
				resp, err := a.sendCompletion(context.Background(), payload)
				if kind == "safe" {
					if err != nil {
						t.Fatalf("safe field refused: %v", err)
					}
					resp.Body.Close()
					if calls.Load() != before+1 {
						t.Fatal("safe request was not sent exactly once")
					}
				} else {
					if !errors.Is(err, errAriaCompletionInput) || resp != nil {
						t.Fatal("sensitive input was not refused")
					}
					if calls.Load() != before {
						t.Fatal("refused request reached HTTP")
					}
				}
			})
		}
	}
	// Credential keys in a future object must retain their key/value context.
	before := calls.Load()
	_, err := a.sendCompletion(context.Background(), completionPayload(t, map[string]any{"messages": []any{}, "future": map[string]any{"password": "fixture"}}))
	if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
		t.Fatal("future credential object was sent")
	}
}

func TestCompletionBoundarySizeAndErrors(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	// Use spaces so the fixture does not accidentally test opaque-token refusal.
	base := completionPayload(t, map[string]any{"messages": ""})
	for _, size := range []int{ariaJevMaxTailBytes - 1, ariaJevMaxTailBytes, ariaJevMaxTailBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := completionPayload(t, map[string]any{"messages": strings.Repeat(" ", size-len(base))})
			if len(payload) != size {
				t.Fatal("invalid size fixture")
			}
			before := calls.Load()
			resp, err := a.sendCompletion(context.Background(), payload)
			if size <= ariaJevMaxTailBytes {
				if err != nil {
					t.Fatal("bounded request refused")
				}
				resp.Body.Close()
				if calls.Load() != before+1 {
					t.Fatal("bounded request not sent")
				}
			} else if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
				t.Fatal("oversize request sent")
			}
		})
	}
	for _, raw := range []string{"{", "null", `{}`, `{"messages":[],"future":"` + strings.Repeat(" ", ariaCompletionStaticMaxBytes) + `"}`} {
		before := calls.Load()
		if _, err := a.sendCompletion(context.Background(), []byte(raw)); !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
			t.Fatal("invalid or oversized request sent")
		}
	}
	for _, system := range []any{ariaSystemPrompt, []map[string]any{{"type": "text", "text": ariaSystemPrompt, "cache_control": map[string]any{"type": "ephemeral"}}}} {
		body := map[string]any{"messages": "Review", "system": system, "tools": ariaToolsWithCacheBreakpoint(ariaAutonomousTools())}
		resp, err := a.sendCompletion(context.Background(), completionPayload(t, body))
		if err != nil {
			t.Fatalf("compiled instructions rejected: %v", err)
		}
		resp.Body.Close()
	}
	// A field named system is not automatically exempt from the input budget.
	if ariaCompletionInputAllowed(completionPayload(t, map[string]any{"messages": []any{}, "system": strings.Repeat(" ", ariaJevMaxTailBytes)})) {
		t.Fatal("unknown system content bypassed budget")
	}
}

func TestCompletionRoutes(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	for _, route := range []string{"dryrun-title", "dryrun-tail", "dryrun-instruction", "turn"} {
		for _, sensitive := range []bool{false, true} {
			t.Run(route+"/"+strconv.FormatBool(sensitive), func(t *testing.T) {
				value := "Review this change"
				if sensitive {
					value = "api_key=fixture-value"
				}
				before := calls.Load()
				var err error
				if strings.HasPrefix(route, "dryrun-") {
					fields := []string{"Fixture", "Waiting", "Review"}
					switch route {
					case "dryrun-title":
						fields[0] = value
					case "dryrun-tail":
						fields[1] = value
					case "dryrun-instruction":
						fields[2] = value
					}
					_, err = a.dryRun(context.Background(), fields[0], fields[1], fields[2], "")
				} else {
					err = a.runTurn(context.Background(), ariaQueryReq{Message: value, Autonomous: true}, func(map[string]any) {})
				}
				if sensitive {
					if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
						t.Fatal("route sent refused input")
					}
				} else if err != nil || calls.Load() != before+1 {
					t.Fatalf("safe route failed: %v", err)
				}
			})
		}
	}
}

func TestCompletionNoVerdictSendsNothing(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	for _, ent := range []*teamEntitlements{nil, newTeamEntitlements(t.TempDir(), nil)} {
		jevEntitlementsRef.Store(ent)
		if teamCloudLLMAllowed() {
			t.Fatal("unknown verdict allowed completion")
		}
		if _, err := a.sendCompletion(context.Background(), completionPayload(t, map[string]any{"messages": "Review"})); !errors.Is(err, errAriaCompletionInput) {
			t.Fatal("unknown verdict accepted by outbound boundary")
		}
		if calls.Load() != 0 {
			t.Fatal("unknown verdict reached HTTP")
		}
	}
}

// Inventory the outbound boundary itself, not a list of known call-site files:
// a new direct caller with this endpoint fails even in a newly added Go file.
func TestCompletionEndpointHasOneBoundary(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	senders := map[string]bool{}
	endpoints := 0
	for _, name := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, _ := strconv.Unquote(lit.Value)
				if strings.Contains(value, "/api/aria/complete") {
					endpoints++
					if name != "aria_completion.go" {
						t.Errorf("completion endpoint bypass in %s", name)
					}
				}
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "sendCompletion" {
					senders[name] = true
				}
			}
			return true
		})
	}
	if endpoints != 1 {
		t.Errorf("completion endpoint count=%d, want one", endpoints)
	}
	if len(senders) != 2 {
		t.Errorf("completion caller count=%d, want two", len(senders))
	}
	for _, name := range []string{"aria.go", "aria_dryrun.go"} {
		if !senders[name] {
			t.Errorf("%s no longer uses completion boundary", name)
		}
	}
}

func TestNativeCautiousCompletionBoundary(t *testing.T) {
	completionTestVerdict(t, true)
	for _, sensitive := range []bool{false, true} {
		a, calls := completionTestServer(t)
		p := testPaneWithTail(t, "native-completion", "Fixture", "")
		p.Kind = "native"
		input := json.RawMessage(`{"command":"pwd"}`)
		if sensitive {
			input = json.RawMessage(`{"command":"echo password=fixture"}`)
		}
		ap := &NativeApproval{RequestID: "fixture-request", ToolName: "Bash", RawInput: input}
		p.setNativePending(ap)
		a.mgr = testManagerWithPane(p)
		if fired, _ := a.runNativeApprovalTurn(context.Background(), p.ID, p.Title, ap, "Review", "automode_cautious", 0, true); fired {
			t.Fatal("text-only or refused decision approved a permission")
		}
		if p.peekNativePending(ap.RequestID) == nil {
			t.Fatal("permission did not remain for the human")
		}
		want := int32(1)
		if sensitive {
			want = 0
		}
		if calls.Load() != want {
			t.Fatalf("native cautious calls=%d, want %d", calls.Load(), want)
		}
	}
}

func TestNativeCompletionChecksStructuredArguments(t *testing.T) {
	completionTestVerdict(t, true)
	for _, tc := range []struct {
		name, tool, raw string
		allowed         bool
	}{
		{"safe Write", "Write", `{"file_path":"file.txt","content":"ordinary text"}`, true},
		{"safe Bash", "Bash", `{"command":"printf hello"}`, true},
		{"safe duplicate field", "Write", `{"content":"one","content":"two"}`, true},
		{"Write JSON key", "Write", `{"file_path":"file.txt","content":"{\"api_key\":\"fixture-value\"}"}`, false},
		{"Write escaped JSON key", "Write", `{"content":"{\"\\u0061pi_key\":\"fixture-value\"}"}`, false},
		{"Write numeric credential", "Write", `{"content":"{\"password\":123456}"}`, false},
		{"Write large numeric credential", "Write", `{"content":"{\"password\":1e500}"}`, false},
		{"Bash JSON body", "Bash", `{"command":"curl -d '{\"password\":\"fixture-value\"}'"}`, false},
		{"duplicate hides credential", "Bash", `{"command":"curl -d '{\"password\":\"fixture-value\"}'","command":"pwd"}`, false},
		{"malformed arguments", "Write", `{`, false},
		{"oversize whole arguments", "Write", `{"content":"` + strings.Repeat(" ", ariaJevMaxTailBytes) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := completionTestServer(t)
			p := testPaneWithTail(t, "native-structured", "Fixture", "")
			p.Kind, p.AutoMode = "native", "cautious"
			ap := &NativeApproval{RequestID: "fixture-request", ToolName: tc.tool, RawInput: json.RawMessage(tc.raw)}
			p.setNativePending(ap)
			a.mgr = testManagerWithPane(p)
			fired, evaluated := a.runNativeApprovalTurn(context.Background(), p.ID, p.Title, ap, "Review", "automode_cautious", 0, true)
			if fired || p.peekNativePending(ap.RequestID) == nil {
				t.Fatal("synthetic native approval did not stay with the person")
			}
			want := int32(0)
			if tc.allowed {
				want = 1
			}
			if calls.Load() != want || evaluated != tc.allowed {
				t.Fatal("structured argument check did not control the native request")
			}
		})
	}
}

func TestCompletionDecodesJSONEmbeddedInStrings(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	for _, tc := range []struct {
		name, text string
		allowed    bool
	}{
		{"safe JSON", `Input: {"content":"ordinary text"}`, true},
		{"safe large JSON number", `Input: {"value":1e500}`, true},
		{"ordinary shell braces", `Input: echo {one,two}`, true},
		{"nested Write", `Input: {"content":"{\"api_key\":\"fixture-value\"}"}`, false},
		{"nested Bash", `Input: {"command":"curl -d '{\"password\":\"fixture-value\"}'"}`, false},
		{"duplicate embedded field", `Input: {"command":"curl -d '{\"password\":\"fixture-value\"}'","command":"pwd"}`, false},
		{"numeric JSON key", `Input: {"content":"{\"password\":123456}"}`, false},
		{"large numeric JSON key", `Input: {"content":"{\"password\":1e500}"}`, false},
		{"encoded key", `Input: {"content":"{\"\\u0070assword\":\"fixture-value\"}"}`, false},
		{"opaque JSON value", `Input: {"content":"{\"value\":\"Q7n2V8m4L9r3X6k1T5c0B2d8W4f6Z9p3\"}"}`, false},
		{"parse budget", strings.Repeat("{x ", 1025), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			payload := completionPayload(t, map[string]any{"messages": []ariaMessage{{Role: "user", Content: []ariaBlock{{Type: "text", Text: tc.text}}}}})
			resp, err := a.sendCompletion(context.Background(), payload)
			if tc.allowed {
				if err != nil || calls.Load() != before+1 {
					t.Fatal("safe formatted evidence was refused")
				}
				resp.Body.Close()
			} else if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
				t.Fatal("encoded sensitive evidence or check exhaustion reached HTTP")
			}
		})
	}
	// Each layer is valid JSON, as when a tool writes a JSON string literal.
	text := `{"api_key":"fixture-value"}`
	for i := 0; i < 4; i++ {
		encoded, _ := json.Marshal(text)
		text = string(encoded)
		before := calls.Load()
		_, err := a.sendCompletion(context.Background(), completionPayload(t, map[string]any{"messages": "Input: " + text}))
		if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
			t.Fatal("nested JSON string literal reached HTTP")
		}
	}
	var deep any = "ordinary text"
	for i := 0; i < 70; i++ {
		deep = []any{deep}
	}
	before := calls.Load()
	_, err := a.sendCompletion(context.Background(), completionPayload(t, map[string]any{"messages": deep}))
	if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
		t.Fatal("decode depth exhaustion reached HTTP")
	}
	secretMessage, _ := json.Marshal(`Input: {"content":"{\"api_key\":\"fixture-value\"}"}`)
	payload := []byte(`{"messages":` + string(secretMessage) + `,"messages":"Review"}`)
	_, err = a.sendCompletion(context.Background(), payload)
	if !errors.Is(err, errAriaCompletionInput) || calls.Load() != before {
		t.Fatal("duplicate request field hid sensitive evidence")
	}
}

func TestTeamCautiousCompletionRefusalLeavesHuman(t *testing.T) {
	for _, field := range []string{"title", "tail"} {
		t.Run(field, func(t *testing.T) {
			a, p, frames, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
			completionTestVerdict(t, true)
			a.teamFeatures = func() bool { return true }
			a.settings = &ariaSettingsStore{}
			t.Setenv(ariaJevAutoEnv, "0")
			fixture, calls := completionTestServer(t)
			base, tok := fixture.cloudCreds()
			a.setCloudCreds(base, tok)
			if field == "title" {
				p.Title = "password=fixture"
			} else {
				menu := strings.Replace(cautiousRenderedCodexMenu, "$ pwd", "$ printf password=fixture", 1)
				appendDirectAutoMenuRedraw(t, p, menu)
				seedGridMenu(t, p, menu)
			}
			for i := 0; i < 3; i++ {
				a.evaluateObservation(context.Background(), p.ID, p.Title, tailOfPane(p, 30))
			}
			if a.BackoffStreak(p.ID) != 0 || a.metrics.AriaEvalErrorsTotal.Load() != 0 {
				t.Fatal("local refusal counted as a provider failure")
			}
			outcomes := a.recentEvalOutcomes(p.ID)
			if len(outcomes) == 0 || outcomes[0].Kind != "declined" {
				t.Fatal("local refusal was not left for human review")
			}
			if calls.Load() != 0 {
				t.Fatal("cautious refusal reached cloud")
			}
			expectNoFrame(t, frames, "refused completion input")
		})
	}
}

func TestCompletionPermissionAttemptEndsTurn(t *testing.T) {
	for _, outcome := range []string{"fired", "held", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			completionTestVerdict(t, true)
			a, p, frames, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
			a.teamFeatures = func() bool { return true }
			a.settings = &ariaSettingsStore{}
			t.Setenv(ariaJevAutoEnv, "0")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/aria/complete" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				// A provider may return long thinking and an opaque signature. These
				// are not another permission request and never need uploading again.
				events := []any{
					map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": strings.Repeat("reasoning ", 600), "signature": strings.Repeat("Ab1C", 32)}},
					map[string]any{"type": "content_block_stop", "index": 0},
					map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "fixture-tool", "name": "send_input"}},
					map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(completionPayload(t, map[string]any{"pane_id": p.ID, "input": "1\n"}))}},
					map[string]any{"type": "content_block_stop", "index": 1},
					map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}},
					map[string]any{"type": "message_stop"},
				}
				for _, event := range events {
					raw, _ := json.Marshal(event)
					_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
				}
			}))
			t.Cleanup(server.Close)
			a.setCloudCreds(server.URL, "synthetic-credential")
			events := a.uiBus.subscribe()
			defer a.uiBus.unsubscribe(events)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); a.evaluateObservation(ctx, p.ID, p.Title, tailOfPane(p, 30)) }()
			confirmed := false
			for !confirmed {
				select {
				case event := <-events:
					if event.Type != "aria-auto-needs-confirm" {
						continue
					}
					var pending ariaAutoPendingConfirm
					if json.Unmarshal([]byte(event.Text), &pending) != nil || pending.ID == "" {
						t.Fatal("invalid confirmation")
					}
					if outcome == "held" {
						atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
						if hold, _ := autoFireUserHold(p, time.Now()); !hold {
							t.Fatal("typing hold fixture is not active")
						}
					}
					if !a.signalConfirm(pending.ID, outcome != "cancelled", false) {
						t.Fatal("could not resolve fixture confirmation")
					}
					confirmed = true
				case <-done:
					t.Fatal("evaluation ended without a tool confirmation")
				case <-ctx.Done():
					t.Fatal("tool confirmation did not arrive")
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("evaluation did not finish")
			}
			if calls.Load() != 1 || a.metrics.AriaEvalErrorsTotal.Load() != 0 {
				t.Fatal("tool result caused a follow-up completion or failure")
			}
			outcomes := a.recentEvalOutcomes(p.ID)
			if len(outcomes) == 0 || outcomes[0].Kind == "error" {
				t.Fatal("tool attempt lost its terminal outcome")
			}
			if outcome == "fired" {
				expectFrame(t, frames, "single completion approval", "\r")
				expectNoFrame(t, frames, "duplicate tool attempt")
			} else {
				expectNoFrame(t, frames, "held or cancelled completion")
			}
			if outcome != "cancelled" && a.BackoffStreak(p.ID) != 0 {
				t.Fatal("tool result bumped the backoff streak")
			}
		})
	}
}

func TestCompletionCompiledInstructionsLeaveRoomForEvidence(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	observation := strings.Repeat("Read the local folder contents.\n", 90)
	if _, err := a.dryRun(context.Background(), "Fixture", observation, ariaAutoModeCautiousInstruction, ""); err != nil {
		t.Fatalf("ordinary cautious evidence refused: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("ordinary cautious evidence did not reach its route")
	}
	// A future field does not gain the message template's budget exemption.
	payload := completionPayload(t, map[string]any{"messages": "Review", "future": strings.Repeat(ariaAutoModeCautiousInstruction, 3)})
	if ariaCompletionInputAllowed(payload) {
		t.Fatal("future field bypassed variable budget")
	}
}

func TestCompletionRefusalMemoRearms(t *testing.T) {
	completionTestVerdict(t, true)
	a, p, _, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	a.teamFeatures = func() bool { return true }
	_, policy := withAriaAutoModePolicy(context.Background(), p)
	remember := func(on bool) {
		pos, hash := completionPromptIdentity(p)
		a.rememberCompletionRefusal(p, policy, on, pos, hash)
	}
	remember(true)
	if !a.completionRefused(p) {
		t.Fatal("same permission was not memoized")
	}
	oldPos, oldHash := completionPromptIdentity(p)
	menu := strings.Replace(cautiousRenderedCodexMenu, "$ pwd", "$ ls", 1)
	appendDirectAutoMenuRedraw(t, p, menu)
	seedGridMenu(t, p, menu)
	a.rememberCompletionRefusal(p, policy, true, oldPos, oldHash)
	if a.completionRefused(p) {
		t.Fatal("a new action was suppressed")
	}
	remember(true)
	p.SetAutoMode("")
	p.SetAutoMode("cautious")
	if a.completionRefused(p) {
		t.Fatal("new policy was suppressed")
	}
	_, policy = withAriaAutoModePolicy(context.Background(), p)
	remember(false)
	if a.completionRefused(p) {
		t.Fatal("new positive operator verdict was suppressed")
	}
	remember(true)
	a.OnPaneExit(p.ID)
	if a.completionRefused(p) {
		t.Fatal("exited pane retained a refusal")
	}
	a.rememberCompletionRefusal(p, policy, true, 0, 0)
	if a.completionRefused(p) {
		t.Fatal("missing terminal prompt was memoized")
	}
}

func TestNativeCompletionRefusalIsNeutral(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	p := testPaneWithTail(t, "native-neutral", "Fixture", "")
	p.Kind = "native"
	p.AutoMode = "cautious"
	p.setNativePending(&NativeApproval{RequestID: "fixture-request", ToolName: "Bash", RawInput: json.RawMessage(`{"command":"echo password=fixture"}`)})
	a.mgr = testManagerWithPane(p)
	a.evalLog = newAriaEvalLog()
	a.metrics = NewMetrics()
	for i := 0; i < 3; i++ {
		a.evaluateNativeObservation(context.Background(), p.ID, p.Title, p, "Review", "automode_cautious", 0, true)
	}
	if calls.Load() != 0 || a.BackoffStreak(p.ID) != 0 || a.metrics.AriaEvalErrorsTotal.Load() != 0 {
		t.Fatal("native local refusal sent or counted a provider failure")
	}
	if !p.hasNativePending() || !a.completionRefused(p) {
		t.Fatal("native refusal was not retained for human review")
	}
}

func TestNativeCompletionRefusalEndsWhenPersonAnswers(t *testing.T) {
	completionTestVerdict(t, true)
	a, calls := completionTestServer(t)
	p, agentEnd := nativePaneForApproval(t, "native-refusal-answer")
	p.AutoMode = "cautious"
	ap := &NativeApproval{RequestID: "refused-request", ToolUseID: "refused-tool", ToolName: "Bash", RawInput: json.RawMessage(`{"command":"echo password=fixture"}`)}
	p.setNativePending(ap)
	a.mgr = testManagerWithPane(p)
	if fired, evaluated := a.runNativeApprovalTurn(context.Background(), p.ID, p.Title, ap, "Review", "automode_cautious", 0, true); fired || evaluated || !a.completionRefused(p) {
		t.Fatal("native local refusal was not retained")
	}
	position := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	events := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(events)
	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		startAriaWatcher(ctx, a.mgr, a.uiBus, a)
	}()
	defer func() { cancel(); <-watchDone }()
	// Synchronize the watcher subscription before publishing the real state edge.
	deadline := time.Now().Add(time.Second)
	for {
		a.uiBus.mu.RLock()
		ready := len(a.uiBus.subscribers) == 2
		a.uiBus.mu.RUnlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	readDone := make(chan error, 1)
	go func() {
		_ = agentEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := ReadFrame(agentEnd)
		readDone <- err
	}()
	if _, err := p.resolveApproval(ap.RequestID, false, ""); err != nil {
		t.Fatal("person could not answer the synthetic approval")
	}
	if err := <-readDone; err != nil {
		t.Fatal("synthetic agent did not receive the answer")
	}
	if atomic.LoadInt64(&p.LastAckedChoicePromptPos) != position {
		t.Fatal("fixture unexpectedly advanced the approval position")
	}
	if a.completionRefused(p) {
		t.Fatal("answered request still suppressed observations")
	}
	_, _ = p.buffer.Write([]byte("The next turn needs input?\n"))
	a.uiBus.publish(uiEvent{Type: "pane-state", PaneID: p.ID, PaneTitle: p.Title, ToState: string(StateAwaitingInput)})
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case evt := <-events:
			if evt.Type == "aria-observation" {
				if evt.ToState != string(StateAwaitingInput) || !strings.Contains(evt.Text, "The next turn needs input?") {
					t.Fatal("watcher retained a stale approval observation")
				}
				if calls.Load() != 0 {
					t.Fatal("local refusal or next input observation sent a completion")
				}
				return
			}
		case <-timer.C:
			t.Fatal("watcher dropped the post-answer observation")
		}
	}
}

func TestNativeCompletionRefusalDoesNotBlockSiblingRetry(t *testing.T) {
	completionTestVerdict(t, true)
	a, _ := completionTestServer(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aria/complete" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	a.setCloudCreds(server.URL, "synthetic-credential")
	p := testPaneWithTail(t, "native-refusal-siblings", "Fixture", "")
	p.Kind, p.AutoMode = "native", "cautious"
	refused := &NativeApproval{RequestID: "a-refused", ToolName: "Bash", RawInput: json.RawMessage(`{"command":"echo password=fixture"}`)}
	retry := &NativeApproval{RequestID: "b-retry", ToolName: "Bash", RawInput: json.RawMessage(`{"command":"ls"}`)}
	p.setNativePending(refused)
	p.setNativePending(retry)
	a.mgr = testManagerWithPane(p)
	a.sweepStreaks = map[string]*sweepStreak{}
	for i := int32(1); i <= 2; i++ {
		a.evaluateNativeObservation(context.Background(), p.ID, p.Title, p, "Review", "automode_cautious", 0, true)
		if calls.Load() != i {
			t.Fatal("sibling's transient failure was not retried exactly once")
		}
		if !a.nativeCompletionRefused(p, refused.RequestID) || a.nativeCompletionRefused(p, retry.RequestID) || a.completionRefused(p) {
			t.Fatal("local refusal suppressed an unrefused sibling")
		}
	}
	p.takeNativePending(retry.RequestID)
	if !a.completionRefused(p) {
		t.Fatal("unchanged refused request was not suppressed")
	}
	p.takeNativePending(refused.RequestID)
	if a.completionRefused(p) {
		t.Fatal("empty pending set retained a refusal")
	}
}

func TestCompletionModelDeclineStaysOutOfAttentionAndPush(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run("native="+strconv.FormatBool(native), func(t *testing.T) {
			completionTestVerdict(t, true)
			t.Setenv(ariaJevAutoEnv, "0")
			t.Setenv("SOLU_ARIA_NATIVE_AUTO", "1")
			const marker = "MODEL_DECLINE_MARKER"
			modelReason := marker + strings.Repeat(" untrusted model explanation", 100)
			delta, err := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": modelReason}})
			if err != nil {
				t.Fatal("could not encode synthetic model reply")
			}
			var calls atomic.Int32
			pushBodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/aria/complete":
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"+
						"data: "+string(delta)+"\n\n"+
						"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
						"data: {\"type\":\"message_stop\"}\n\n")
				case "/api/internal/notify":
					body, _ := io.ReadAll(r.Body)
					pushBodies <- body
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			t.Cleanup(server.Close)
			var a *Aria
			var p *Pane
			if native {
				a, _ = completionTestServer(t)
				p = testPaneWithTail(t, "native-model-decline", "Fixture", "")
				p.Kind, p.AutoMode = "native", "cautious"
				p.setNativePending(&NativeApproval{RequestID: "fixture-request", ToolName: "Read", RawInput: json.RawMessage(`{"file_path":"file.txt"}`)})
				a.mgr = testManagerWithPane(p)
			} else {
				a, p, _, _ = cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
			}
			a.teamFeatures = func() bool { return true }
			a.settings = &ariaSettingsStore{}
			a.evalLog = newAriaEvalLog()
			a.sweepStreaks = map[string]*sweepStreak{}
			a.setCloudCreds(server.URL, "synthetic-credential")
			a.attention = newAttentionEngine(a.mgr)
			a.attention.SetAria(a)
			if native {
				a.evaluateNativeObservation(context.Background(), p.ID, p.Title, p, "Review", "automode_cautious", 0, true)
			} else {
				a.evaluateObservation(context.Background(), p.ID, p.Title, tailOfPane(p, 30))
			}
			if calls.Load() != 1 {
				t.Fatal("fixture did not evaluate a model decline")
			}
			outcomes := a.recentEvalOutcomes(p.ID)
			if len(outcomes) == 0 || outcomes[0].Kind != "declined" || !strings.Contains(outcomes[0].Reason, marker) || len(outcomes[0].Reason) > ariaEvalLogReasonMax {
				t.Fatal("model explanation was not retained in the bounded evaluation record")
			}
			if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != ariaModelDeclineReason {
				t.Fatal("attention did not use a fixed local decline reason")
			}
			pusher := &Pusher{cloudBase: server.URL, token: "synthetic-credential", client: server.Client()}
			pusher.fire(p, string(StateChoicePrompt))
			var body []byte
			select {
			case body = <-pushBodies:
			case <-time.After(2 * time.Second):
				t.Fatal("synthetic notification was not delivered")
			}
			var fields map[string]string
			if json.Unmarshal(body, &fields) != nil || fields["reason"] != ariaModelDeclineReason || strings.Contains(string(body), marker) {
				t.Fatal("phone notification included a model explanation")
			}
		})
	}
}
