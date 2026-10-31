package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// ringEpochMs identifies this daemon process's ring-buffer lifetime.
// Ring offsets are only meaningful within one epoch: a daemon restart
// (auto-update) resets every pane's ring, so clients echo the epoch
// back with their `since` offset and a mismatch forces a full replay.
var ringEpochMs = time.Now().UnixMilli()

// WebSocket transport timings are variables so handler tests can compress
// them without changing the production policy.
var (
	wsPingInterval = 30 * time.Second
	wsWriteTimeout = 10 * time.Second
	wsReadDeadline = 90 * time.Second
	// A suspended phone app stops both WebSocket auto-pongs and its 30s
	// app-level {type:"ping"} while the TCP connection can remain healthy.
	// Five minutes tolerates ordinary backgrounding; silent dead connections
	// are still bounded here, while ping writes fail fast on torn connections.
	wsMobileReadDeadline = 5 * time.Minute
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     originProtected,
	// Browsers hard-fail a handshake that offered subprotocols but had
	// none selected. Cloud web clients offer "solu-auth" plus a
	// "bearer.<jwt>" entry the cloud consumes (and strips before
	// forwarding); echoing the harmless name here completes the
	// negotiation without ever reflecting a credential.
	Subprotocols: []string{"solu-auth"},
}

// Incoming control frames from the browser.
type wsClientMsg struct {
	Token   string `json:"token,omitempty"`
	Ready   bool   `json:"ready,omitempty"`
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
	Visible *bool  `json:"visible,omitempty"`
	// Reason qualifies a "claim":
	//   - "takeover" is the explicit overlay-button gesture: always granted,
	//     stamps sizing.
	//   - "first-open" is the desktop's first-attach auto-claim: stamps
	//     sizing when granted, but is subject to claim arbitration — it
	//     cannot displace a live owner whose user is at their device
	//     (ClaimWithReason).
	//   - "reconnect" means a desktop is merely re-asserting input ownership
	//     after one of its frequent WebView2 reconnects: never re-stamps the
	//     active-sizing seq, and is subject to claim arbitration.
	//   - "adopt-unowned" is the conditional reclaim a previous owner sends
	//     after a connection drop (daemon-update restart): granted only when
	//     no sub owns the pane (AdoptIfUnowned), never restamps sizing.
	//   - empty is legacy/ambiguous (old bundles, the Flutter app): treated
	//     as automatic within claimConnectGrace of connect (arbitrated, no
	//     sizing stamp), as an explicit takeover after it.
	Reason string `json:"reason,omitempty"`
}

// wsInputPolicy is decided once per pane socket from the daemon's own
// classification of the caller (apiServer.paneSocketInputPolicy).
type wsInputPolicy struct {
	// allowed decides whether keystrokes may reach the pane while it shows a
	// permission prompt (apiServer.promptInputGate). Nil allows.
	allowed func(*Pane) bool
	// human is true only when identifyCaller classified this socket as
	// callerHuman (a traced browser, or a relayed request that passed the
	// device gate). Only a human socket may adopt an unowned pane by typing
	// into it; nothing the client sends can set this.
	human bool
	// kind names the caller class for logs ("human", "pane", ...).
	kind string
}

// wsDropLogInterval and wsDropAckInterval rate-limit, per socket, the log
// line and the truthful ownership frame sent when keystrokes are dropped.
var (
	wsDropLogInterval = 10 * time.Second
	wsDropAckInterval = 2 * time.Second
)

// isUserKeypress reports whether data is plausibly a person typing rather than
// terminal machinery: anything except a multi-byte ESC-prefixed sequence
// (DA/OSC auto-replies, mouse/focus reports). A lone ESC is a real keypress.
func isUserKeypress(data []byte) bool {
	return len(data) > 0 && !(data[0] == 0x1b && len(data) > 1)
}

// isTerminalReply reports whether data starts with a sequence a terminal
// emulator sends on its own rather than a key a person pressed: OSC/DCS/APC/PM
// strings, focus and mouse reports, and CSI query answers (DA, CPR, DSR,
// DECRPM, window reports). Arrow and function keys, Alt combinations and
// bracketed pastes (ESC[200~) are not replies. Shift+F3 encodes like a CPR
// (ESC[1;2R) and is treated as a reply; the next key adopts instead.
func isTerminalReply(data []byte) bool {
	if len(data) < 2 || data[0] != 0x1b {
		return false
	}
	switch data[1] {
	case ']', 'P', '_', '^':
		return true
	case '[':
	default:
		return false
	}
	if len(data) >= 3 && (data[2] == 'M' || data[2] == '<') {
		return true // X10 / SGR mouse report
	}
	if len(data) == 3 && (data[2] == 'I' || data[2] == 'O') {
		return true // focus in / out
	}
	// Find the CSI final byte (0x40–0x7e) after parameters/intermediates.
	for i := 2; i < len(data); i++ {
		b := data[i]
		if b < 0x40 || b > 0x7e {
			continue
		}
		switch b {
		case 'c': // DA1/DA2/DA3 answers carry a ?, > or = prefix
			return len(data) > 2 && (data[2] == '?' || data[2] == '>' || data[2] == '=')
		case 'R', 'n', 't':
			return true // cursor position, device status, window report
		case 'y':
			return i > 2 && data[i-1] == '$' // DECRPM mode report
		}
		return false
	}
	return false
}

// handleWS serves a pane's terminal stream. policy, when given, classifies
// each connection (see wsInputPolicy); without it the socket is treated as a
// non-human viewer whose input is never held by a prompt gate.
func handleWS(m *Manager, policy ...func(*http.Request) wsInputPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p, ok := m.Get(id)
		if !ok {
			http.Error(w, "pane not found", http.StatusNotFound)
			return
		}
		inputAllowed := func(*Pane) bool { return true }
		var inPolicy wsInputPolicy
		if len(policy) > 0 && policy[0] != nil {
			inPolicy = policy[0](r)
			if inPolicy.allowed != nil {
				inputAllowed = inPolicy.allowed
			}
		}
		if inPolicy.kind == "" {
			inPolicy.kind = "unclassified"
		}
		heldInput := false
		holdInput := func() bool {
			if inputAllowed(p) {
				return false
			}
			if !heldInput {
				heldInput = true
				log.Printf("authority: held an agent's keystrokes for pane %s: it is showing a permission prompt", id)
			}
			return true
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade: %v", err)
			return
		}

		// Parse initial viewport from query string. Used by Subscribe
		// so this sub's starting cols/rows are known — but only the
		// owner's cols actually drive the PTY size.
		var initCols, initRows uint16
		q := r.URL.Query()
		if q.Get("cols") != "" && q.Get("rows") != "" {
			initCols, initRows, _ = parseTerminalQueryDimensions(q.Get("cols"), q.Get("rows"))
		}

		// The client's actual terminal size, which may differ from the
		// cols/rows it seeds the PTY with. Used only to line up the
		// reattach repaint (see reattachPaintRowOffset).
		var termCols, termRows uint16
		if q.Get("term_cols") != "" && q.Get("term_rows") != "" {
			termCols, termRows, _ = parseTerminalQueryDimensions(q.Get("term_cols"), q.Get("term_rows"))
		}

		skipReplay := q.Get("skip_replay") == "1" || q.Get("skipReplay") == "1"

		// Exact-resume negotiation. A reconnecting client that tracked
		// its ring offset sends since=<offset>&epoch=<ringEpochMs>. The
		// epoch ties the offset to THIS daemon process's ring — after a
		// daemon restart the ring (and its offsets) reset, so a stale
		// epoch invalidates the offset and forces a full replay instead
		// of resuming from a meaningless position.
		mode := subModeFull
		var since int64
		if skipReplay {
			mode = subModeNone // legacy: keep surface, best-effort repair
			if sinceStr := q.Get("since"); sinceStr != "" {
				if v, err := strconv.ParseInt(sinceStr, 10, 64); err == nil && v >= 0 &&
					q.Get("epoch") == strconv.FormatInt(ringEpochMs, 10) {
					mode = subModeResume
					since = v
				} else {
					// Offset unusable (restarted daemon / bad value):
					// full replay so the client resets to a known state
					// rather than silently keeping a stale surface.
					mode = subModeFull
				}
			}
		}
		mobile := q.Get("mobile") == "1"
		s := p.subscribeStream(initCols, initRows, mode, since, subOptions{
			initiallyHidden:    q.Get("visible") == "0",
			mobile:             mobile,
			skipSyntheticPaint: mobile && q.Get("client") != "web",
			// Persistent per-device identity — lets AdoptIfUnowned recognize
			// "the owner is this same device's abandoned socket" and displace
			// it instantly instead of denying with a false "in use somewhere
			// else". Bounded so a hostile query string can't bloat sub state.
			deviceID:           truncateString(q.Get("device"), 64),
			retirementViewerID: ownerViewerClientID(q.Get("retirement_viewer")),
			// Driven browsers (navigator.webdriver) self-identify so they
			// never vote on PTY sizing (see subEntry.automation).
			automation: q.Get("automation") == "1",
			termCols:   termCols,
			termRows:   termRows,
		})
		snapshot, ch, ctrl := s.snapshot, s.ch, s.ctrl
		updateSize, updateVisibility, isOwner := s.updateSize, s.updateVisibility, s.isOwner
		markSeen, markActive, markInput := s.markSeen, s.markActive, s.markInput
		subID := s.id
		defer s.cancel()

		// ownsInput reports whether this socket may write keystrokes to the
		// PTY. A non-owner's keystrokes are dropped — accidental taps on an
		// un-claimed mobile view must not send commands — EXCEPT that a
		// visible human viewer typing into a pane nobody owns adopts it first
		// (AdoptOnInput: atomic, never displaces any owner). Every drop is
		// logged rate-limited (pane, viewer, reason — never the bytes) and
		// answered with this socket's truthful ownership frame so the client
		// can show the claim banner instead of losing input silently.
		// Terminal replies never adopt (isTerminalReply): a viewer's emulator
		// emits those with nobody at the keyboard. Keys, arrows, Alt combos
		// and bracketed pastes do.
		var lastDropLog, lastDropAck time.Time
		ownsInput := func(data []byte) bool {
			if isOwner() {
				return true
			}
			reason := "viewer-not-human"
			if inPolicy.human {
				reason = "unowned-terminal-reply"
				if len(data) > 0 && !isTerminalReply(data) {
					var granted bool
					if granted, reason = p.AdoptOnInput(subID); granted {
						return true
					}
				}
			}
			now := time.Now()
			if now.Sub(lastDropLog) >= wsDropLogInterval {
				lastDropLog = now
				log.Printf("input: dropped keystrokes on pane %s from viewer sub=%d kind=%s reason=%s",
					p.ID, subID, inPolicy.kind, reason)
			}
			if now.Sub(lastDropAck) >= wsDropAckInterval {
				lastDropAck = now
				p.sendOwnership(subID, false)
			}
			return false
		}

		// Send replay buffer as a single binary frame. Announce it first
		// so reconnecting browser clients can replace their existing
		// xterm buffer instead of appending the replay onto already-
		// rendered history.
		//
		// Strip DA / CPR query sequences before sending. These are
		// one-shot terminal handshakes (ESC[c, ESC[>c, ESC[6n …) that
		// already got a response during the original session. Replaying
		// them causes xterm.js to fire fresh `onData` replies which
		// arrive in the PTY as unexpected bytes, producing the
		// '1;2c1;2c…' echo on bash prompts after Claude exits.
		// Legacy skip-replay (subModeNone) sends nothing here; its gap
		// repair arrives later through the fanout channel as a deferred
		// grid repaint (see Pane.scheduleSubRepair). Full replays and
		// exact resumes announce themselves with replay-start: `resume`
		// tells the client whether to apply in place (exact missed
		// bytes) or reset-and-replace; `offset`+`epoch` give it the
		// baseline to count from so its NEXT reconnect can resume.
		//
		// Announced even when the snapshot is EMPTY (bytes:0): right after
		// a daemon restart the fresh ring may not have consumed the agent's
		// reattach replay yet. Staying silent left a reconnecting client
		// (stale epoch → full replay) holding its old surface with no reset
		// signal — and the agent's replay then FANNED OUT onto that stale
		// surface as if it were live continuation bytes, jumbling the pane.
		// The bytes:0 announce lets the client reset and adopt the fresh
		// (offset, epoch) baseline so the fanned-out history paints onto a
		// clean grid, exactly like a first attach. Old bundles read it as a
		// baseline update and ignore the empty replay — harmless.
		if mode != subModeNone {
			clean := stripTerminalQuerySequences(snapshot)
			meta, _ := json.Marshal(map[string]any{
				"type":   "replay-start",
				"bytes":  len(clean),
				"offset": s.cutOffset,
				"epoch":  ringEpochMs,
				"resume": s.resumed,
			})
			if err := conn.WriteMessage(websocket.TextMessage, meta); err != nil {
				_ = conn.Close()
				return
			}
			// Chunked replay frames. The client counts the replay down
			// against the byte total from replay-start, so it handles
			// multi-frame replays natively — and on first attach each
			// frame becomes its own term.write, giving the browser
			// main thread natural yield points instead of one atomic
			// multi-hundred-KiB parse per pane.
			const replayChunkBytes = 128 * 1024
			for off := 0; off < len(clean); off += replayChunkBytes {
				end := off + replayChunkBytes
				if end > len(clean) {
					end = len(clean)
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, clean[off:end]); err != nil {
					_ = conn.Close()
					return
				}
			}
		}

		// Writer goroutine: fan PTY bytes (binary), control messages
		// (text JSON), and periodic pings → WS. On exit, close the
		// conn so the reader wakes up immediately instead of waiting
		// out the 90s read deadline.
		readerDone := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			defer func() { _ = conn.Close() }()
			ticker := time.NewTicker(wsPingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-readerDone:
					return
				case chunk, ok := <-ch:
					if !ok {
						_ = conn.WriteMessage(websocket.CloseMessage,
							websocket.FormatCloseMessage(websocket.CloseNormalClosure, "pane exited"))
						return
					}
					_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
						return
					}
				case ctrlMsg, ok := <-ctrl:
					if !ok {
						continue
					}
					_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if err := conn.WriteMessage(websocket.TextMessage, ctrlMsg); err != nil {
						return
					}
				case <-ticker.C:
					_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						return
					}
				}
			}
		}()

		// Reader: WS → PTY
		readDeadline := wsReadDeadline
		if mobile {
			readDeadline = wsMobileReadDeadline
		}
		conn.SetReadLimit(1 << 20)
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
			// A pong proves the client is alive even when it's idle (no app
			// frames). Bump sizing liveness so an idle-but-connected viewer is
			// never treated as stale by desiredSizeLocked. (Over the relay the
			// pong is stripped; the app-level frames below cover that path.)
			markSeen()
			return nil
		})

		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Any inbound frame proves the client is alive — reset the read
			// deadline on ALL messages, not just on WS pong (above). Over the
			// cloud relay the WS ping/pong control frames can be stripped by
			// the tunnel, so the daemon would otherwise hit the 90s deadline
			// and close a perfectly live connection — forcing the phone to
			// reconnect. That churn is what flips the shared PTY width
			// (desktop⟷phone) and breaks Codex on mobile. Pairs with the
			// native client's periodic app-level {type:"ping"}.
			_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
			// Any inbound frame also proves sizing liveness (see staleSubWindow).
			markSeen()
			switch mt {
			case websocket.BinaryMessage:
				p.ownerViewerClosing(subID, false)
				// Raw keystrokes in binary. Only the owner can write
				// to the PTY (see ownsInput).
				if !ownsInput(data) || holdInput() {
					continue
				}
				// Real user presence, for claim arbitration. Multi-byte
				// ESC-prefixed data is mostly terminal machinery (DA/OSC
				// auto-replies, mouse/focus reports) — not proof anyone is
				// at the keyboard. A lone ESC is a real keypress.
				if isUserKeypress(data) {
					markInput()
				}
				if _, werr := p.Write(data); werr != nil {
					goto cleanup
				}
			case websocket.TextMessage:
				var msg wsClientMsg
				if jerr := json.Unmarshal(data, &msg); jerr != nil {
					continue
				}
				if msg.Type != "owner-viewer-clean-close" {
					p.ownerViewerClosing(subID, false)
				}
				switch msg.Type {
				case "owner-viewer-clean-close":
					p.ownerViewerClosing(subID, msg.Ready)
				case "owner-retirement-ack":
					p.acknowledgeOwnerRetirement(subID, msg.Token, msg.Ready, msg.Reason)
				case "input":
					if !ownsInput([]byte(msg.Data)) || holdInput() {
						continue
					}
					if isUserKeypress([]byte(msg.Data)) {
						markInput()
					}
					if _, werr := p.Write([]byte(msg.Data)); werr != nil {
						goto cleanup
					}
				case "resize":
					// All subs tell us their cols; only the owner's
					// feed recomputeSize. updateSize tracks per-sub
					// size for when/if they become owner.
					if cols, rows, ok := saneTerminalDimensions(int(msg.Cols), int(msg.Rows)); ok {
						updateSize(cols, rows)
					}
				case "visibility":
					if msg.Visible != nil {
						updateVisibility(*msg.Visible)
					}
				case "claim":
					// Transfer input ownership to this sub. Broadcasts
					// ownership-change messages to every other sub
					// so their UIs swap between overlay and normal.
					// A "reconnect" reclaim takes input back but does NOT
					// re-stamp the active-sizing seq, so it can't steal the
					// PTY width from a phone the user is actively viewing.
					// "adopt-unowned" is the CONDITIONAL variant: a client
					// that owned the pane when its connection dropped
					// (daemon-update restart, network blip) asks for it
					// back, and the daemon grants it only if nobody owns
					// the pane now — so it can never undo a takeover that
					// happened while the claimer was away.
					if msg.Reason == "adopt-unowned" {
						p.AdoptIfUnowned(subID)
					} else {
						p.ClaimWithReason(subID, msg.Reason != "reconnect", msg.Reason)
					}
				case "repaint":
					// Lightweight, reconnect-free repair for codex /
					// ratatui normal-buffer panes after a large resize:
					// a CUP-addressed full paint from the replay
					// emulator, sent to THIS subscriber only. Replaces
					// the client's old strategy of closing the WS to
					// force a full ring replay (multi-hundred-ms visible
					// redraw). Targeting the requester (not a broadcast)
					// keeps one client's resize from repainting every
					// other client's screen. No-op for alt-screen panes.
					p.RepaintGridFor(subID)
				case "redraw":
					// User-triggered "force redraw". Fires only when
					// the user clicks the pane-header button or hits
					// the keybinding — never automatic. Ratatui-based
					// TUIs (codex) sometimes leave ghost / duplicate
					// cells after a stress-resize or workspace
					// switch-back; manually dragging the splitter
					// up-and-down forces a clean repaint, and this
					// message is the programmatic equivalent. Not
					// owner-gated — a SIGWINCH + screen-clear is a
					// local repair, not a state change, so even a
					// passive viewer's redraw click is harmless (at
					// worst the owner sees a brief repaint flicker).
					// Was owner-gated in v0.0.189 but the gate made
					// the button silently no-op when the daemon
					// didn't consider this client the owner.
					log.Printf("pane %s: redraw requested (altScreen=%v)",
						p.ID, p.altScreen)
					p.Redraw()
				case "ping":
					// client heartbeat (markSeen above already bumped liveness)
				case "active":
					// This view just became the user's focused, on-screen pane
					// (window/tab focus). Re-stamp it active for SIZING so the
					// pane the user is actually looking at reclaims the shared
					// PTY width from a background-tile or stale sub — without
					// transferring input ownership (that's {type:"claim"}).
					markActive()
				}
			}
		}
	cleanup:
		// ROOT CAUSE: the reported closures at exact 30s multiples (30.0008s, 2m30.028s, 34m30.0006s) are Go time.Duration strings from logMiddleware (daemon/main.go:2117), which logs GET /api/panes/{id}/io 101 <time.Since(start)> when handleWS RETURNS — not when the socket dies. handleWS's reader loop exits at the true death time (client close frame, FIN/RST, relay yamux-stream teardown, or the 90s read deadline at ws.go:227), falls to the cleanup: label (ws.go:360-362) which does conn.Close() then <-writerDone; but the writer goroutine's select (ws.go:196-221) waits only on ch/ctrl/ticker.C and is NOT woken by conn.Close(), so on an idle pane (a phone viewer) it wakes only at the NEXT 30s ping tick, whose write on the closed conn fails instantly, finally letting the handler return. Logged lifetime = actual death time rounded UP to the ticker's 30s grid; the sub-ms residues are the handler-start→writer-start offset (replay write). Beyond logging, defer s.cancel() also waits on <-writerDone, so a dead sub stays registered up to 30 extra seconds.
		close(readerDone)
		_ = conn.Close()
		<-writerDone
	}
}

// truncateString caps s at max bytes — query-string inputs stored on
// long-lived structs must be bounded.
func truncateString(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func parseTerminalQueryDimensions(colsRaw, rowsRaw string) (uint16, uint16, bool) {
	cols, err := strconv.ParseInt(colsRaw, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	rows, err := strconv.ParseInt(rowsRaw, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return saneTerminalDimensions(int(cols), int(rows))
}

// stripTerminalQuerySequences removes terminal queries that trigger
// automatic responses from xterm.js (DA/CPR/DSR and OSC color queries).
// These are one-shot handshakes — replaying them from the ring buffer
// causes xterm.js to emit fresh `onData` bytes that land in the PTY
// as unexpected input and get echoed by bash as '1;2c' or 'rgb:...'.
//
// CSI final bytes known to trigger replies are stripped:
//
//	'c' (0x63) — device attributes (DA1/DA2/DA3): ESC [ [params] c
//	'n' (0x6e) — device status / cursor position report: ESC [ [params] n
//
// OSC 4 / 10 / 11 / 12 color-query forms containing '?' are stripped.
// OSC SET forms such as ESC ] 11 ; rgb:... ST are kept because they
// restore rendering state without triggering a reply.
func stripTerminalQuerySequences(b []byte) []byte {
	out := make([]byte, 0, len(b))
	i := 0
	for i < len(b) {
		// Look for ESC [ (CSI) or ESC ] (OSC).
		if b[i] != 0x1b || i+1 >= len(b) || (b[i+1] != '[' && b[i+1] != ']') {
			out = append(out, b[i])
			i++
			continue
		}
		if b[i+1] == ']' {
			j := i + 2
			termLen := 0
			for j < len(b) {
				if b[j] == 0x07 {
					termLen = 1
					break
				}
				if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
					termLen = 2
					break
				}
				j++
			}
			if termLen == 0 {
				// Truncated OSC — keep as-is.
				out = append(out, b[i:]...)
				break
			}
			if oscBodyIsColorQuery(b[i+2 : j]) {
				i = j + termLen
				continue
			}
			out = append(out, b[i:j+termLen]...)
			i = j + termLen
			continue
		}
		// Scan to the CSI final byte (0x40–0x7e).
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		if j >= len(b) {
			// Truncated sequence — keep as-is.
			out = append(out, b[i:j]...)
			i = j
			continue
		}
		final := b[j]
		if final == 0x63 || final == 0x6e { // 'c' or 'n'
			// Drop this sequence entirely.
			i = j + 1
			continue
		}
		out = append(out, b[i:j+1]...)
		i = j + 1
	}
	return out
}

func oscBodyIsColorQuery(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	n := 0
	i := 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		n = n*10 + int(body[i]-'0')
		i++
	}
	if i == 0 || i >= len(body) || body[i] != ';' {
		return false
	}
	if n != 4 && n != 10 && n != 11 && n != 12 {
		return false
	}
	for _, c := range body[i+1:] {
		if c == '?' {
			return true
		}
	}
	return false
}
