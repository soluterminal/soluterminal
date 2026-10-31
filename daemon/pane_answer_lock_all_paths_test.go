package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A nonblocking agent connection records every attempted frame byte. A missing
// guard fails the test immediately, rather than hanging on an unread net.Pipe.
type answerLockFrameProbe struct {
	net.Conn
	bytes atomic.Int64
}

func (c *answerLockFrameProbe) Write(b []byte) (int, error) {
	c.bytes.Add(int64(len(b)))
	return len(b), nil
}

func lockWithAnswerProbe(p *Pane, probe *answerLockFrameProbe) {
	p.mu.Lock()
	probe.Conn = p.agentConn
	p.agentConn = probe
	// Keep the otherwise eligible mode: the invariant must not depend on Off
	// masking a missing NeverAutoAnswer check or a previously captured decision.
	p.NeverAutoAnswer = true
	p.mu.Unlock()
}

// One registry exercises the complete set of machine-answer entry points.
// Any new answer entry point belongs here. The invariant is ZERO agent frames,
// regardless of delegation, mode, menu detection, payload shape or transport.
func TestAnswerLockEveryAutoAnswerPathWritesZeroFrames(t *testing.T) {
	terminal := func(t *testing.T, probe *answerLockFrameProbe) (*Aria, *Pane, *apiServer) {
		a, p, _, _ := newDirectAutoRepeatHarness(t, "zero-answer")
		assertDirectAutoPromptReady(t, p, "eligible approval")
		lockWithAnswerProbe(p, probe)
		return a, p, &apiServer{mgr: a.mgr, uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}
	}
	native := func(t *testing.T, probe *answerLockFrameProbe) (*Aria, *Pane, *apiServer) {
		p, _ := nativePaneForApproval(t, "zero-native")
		p.mu.Lock()
		p.AutoMode = "always"
		p.mu.Unlock()
		lockWithAnswerProbe(p, probe)
		p.setNativeAllowlist([]string{"Read"})
		p.processNativeLine([]byte(nativeCanUseToolLine("zero-req", "zero-tool", "Read", `{"file_path":"/tmp/example"}`)), false, false)
		a := testNativeAutoAria(t, p)
		return a, p, &apiServer{mgr: a.mgr, uiBus: a.uiBus, agentAuthority: grantedAgentAuthority(t)}
	}
	noop := func(map[string]any) {}
	cases := []struct {
		name string
		run  func(*testing.T, *answerLockFrameProbe)
	}{
		{"grid-direct", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := terminal(t, c)
			a.directAutoFire(context.Background(), p.ID, p.Title)
		}},
		{"aria-observation", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := terminal(t, c)
			a.evaluateObservation(context.Background(), p.ID, p.Title, "")
		}},
		{"aria-send-input-always", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := terminal(t, c)
			a.toolSendInput(context.Background(), "zero", p.ID, "1\n", 0, 0, false, noop)
		}},
		{"aria-send-input-cautious", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := terminal(t, c)
			p.mu.Lock()
			p.AutoMode = "cautious"
			p.mu.Unlock()
			a.toolSendInput(context.Background(), "zero", p.ID, "1\n", 0, 0, true, noop)
		}},
		{"attention", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := terminal(t, c)
			e := newAttentionEngine(a.mgr)
			e.SetAria(a)
			e.Refresh(p)
			if att, _ := p.AttentionNow(); att != AttentionNeedsUser {
				t.Fatalf("locked prompt attention: %s", att)
			}
		}},
		{"native-allowlist", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, _ := native(t, c)
			p.maybeAutoAllowFromList("zero-req", "Read", "tool")
		}},
		{"native-auto-resolve", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := native(t, c)
			a.nativeAutoResolve(p, "zero-req", "zero-confirm", false)
		}},
		{"native-aria-direct", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := native(t, c)
			a.directAutoFireNative(context.Background(), p.ID, p.Title)
		}},
		{"native-aria-tool", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := native(t, c)
			a.toolResolvePermission(context.Background(), "zero", p.ID, "zero-req", false, noop)
		}},
		{"native-answer-prompt", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, a := native(t, c)
			a.mcpToolAnswerPrompt(map[string]any{"pane_id": p.ID, "option": "allow"})
		}},
		{"native-delegated-http", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, a := native(t, c)
			old := identifyCallerHook
			t.Cleanup(func() { identifyCallerHook = old })
			identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "other-pane"} }
			req := httptest.NewRequest("POST", "/resolve", strings.NewReader(`{"allow":true}`))
			req.SetPathValue("id", p.ID)
			req.SetPathValue("reqId", "zero-req")
			a.handlePaneApprovalResolve(httptest.NewRecorder(), req)
		}},
		{"cross-pane-answer-prompt", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, a := terminal(t, c)
			b, err := json.Marshal(toolCallParams{Name: "answer_prompt", Arguments: map[string]any{"pane_id": p.ID, "option": "1"}})
			if err != nil {
				t.Fatal(err)
			}
			a.mcpCallTool(b, nil, "other-pane")
		}},
		{"mcp-raw-undetected-arrow-enter", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, a := terminal(t, c)
			p.buffer = NewRingBuffer(1024)
			p.shutdownGrid()
			a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\x1b[B\r"}, "other-pane")
		}},
		{"mcp-raw-undetected-short-word", func(t *testing.T, c *answerLockFrameProbe) {
			_, p, a := terminal(t, c)
			p.buffer = NewRingBuffer(1024)
			p.shutdownGrid()
			a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "ok"}, "other-pane")
		}},
		{"jev-cautious", func(t *testing.T, c *answerLockFrameProbe) {
			a, p, _ := newJevFullHarness(t, jevNovelToolMenu)
			p.SetAutoMode("cautious")
			lockWithAnswerProbe(p, c)
			tail := ariaJevPermissionTail(p, true)
			_, pos, fp, ok := currentChoicePromptIdentity(p, tail)
			if !ok {
				t.Fatal("missing Jev prompt")
			}
			proof := &jevAmbiguousProof{dataDir: a.dataDir, mode: "cautious", full: true, classified: true, pos: pos, fingerprint: fp, tailDigest: sha256.Sum256([]byte(ariaJevMenuEvidence(tail, true)))}
			bindJevTestProof(t, a, p, proof)
			a.writeJevEnter(context.Background(), p, proof)
		}},
	}
	for _, payload := range []string{"1", "\x1b[B\r", "yes\r"} {
		cases = append(cases, struct {
			name string
			run  func(*testing.T, *answerLockFrameProbe)
		}{"inject-" + payload, func(t *testing.T, c *answerLockFrameProbe) {
			a := authorityTestServer(t)
			a.agentAuthority = grantedAgentAuthority(t)
			p := switchTestPane("zero-inject", "", []string{"bash"}, "")
			drainConn(t, p)
			a.mgr.panes[p.ID] = p
			lockWithAnswerProbe(p, c)
			old := identifyCallerHook
			t.Cleanup(func() { identifyCallerHook = old })
			identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "other-pane"} }
			body, _ := json.Marshal(injectReq{Text: payload})
			req := httptest.NewRequest("POST", "/inject", strings.NewReader(string(body)))
			req.SetPathValue("id", p.ID)
			rec := httptest.NewRecorder()
			a.injectPane(rec, req)
			if rec.Code != 409 {
				t.Fatalf("locked inject: %d %s", rec.Code, rec.Body.String())
			}
		}})
	}
	for _, mt := range []int{websocket.BinaryMessage, websocket.TextMessage} {
		cases = append(cases, struct {
			name string
			run  func(*testing.T, *answerLockFrameProbe)
		}{map[int]string{websocket.BinaryMessage: "ws-binary", websocket.TextMessage: "ws-json"}[mt], func(t *testing.T, c *answerLockFrameProbe) {
			h := newAdoptTestPane(t, false) // allowed=true models an already delegated socket
			agent := h.dial(t, callerPane, "")
			agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			agent.expectOwnership(t, true, "you")
			lockWithAnswerProbe(h.p, c)
			data := "1"
			if mt == websocket.TextMessage {
				data = `{"type":"input","data":"\u001b[B\r"}`
			}
			agent.send(t, mt, data)
			waitForWSTestSubCount(t, h.p, 0, time.Second) // rejected write closes this socket
		}})
	}
	var total int64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &answerLockFrameProbe{}
			t.Cleanup(func() {
				n := probe.bytes.Load()
				total += n
				if n != 0 {
					t.Errorf("locked path wrote %d bytes to the agent (want ZERO frames)", n)
				}
			})
			tc.run(t, probe)
		})
	}
	if total != 0 {
		t.Fatalf("locked auto-answer paths wrote %d transport bytes; want ZERO frames", total)
	}
}

func TestAnswerLockRawHumanInputStillWorks(t *testing.T) {
	h := newAdoptTestPane(t, false)
	h.p.setAutomaticAnswerSettings("", true)
	human := h.dial(t, callerHuman, "")
	human.send(t, websocket.BinaryMessage, "1")
	h.expectWrite(t, "1")
	human.send(t, websocket.TextMessage, `{"type":"input","data":"y\r"}`)
	h.expectWrite(t, "y\r")
	a := &apiServer{mgr: testManagerWithPane(h.p)}
	old := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = old })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	req := httptest.NewRequest("POST", "/inject", strings.NewReader(`{"text":"1"}`))
	req.SetPathValue("id", h.p.ID)
	rec := httptest.NewRecorder()
	a.injectPane(rec, req)
	if rec.Code != 200 {
		t.Fatalf("human inject: %d %s", rec.Code, rec.Body.String())
	}
	h.expectWrite(t, "1")
}
