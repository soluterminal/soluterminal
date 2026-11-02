package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestAskStore(t *testing.T) (*askStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := newAskStore(t.TempDir(), nil)
	s.now = func() time.Time { return now }
	return s, &now
}

func mustPost(t *testing.T, s *askStore, pane, key, text string) Ask {
	t.Helper()
	a, _, err := s.post(askPost{Pane: pane, Key: key, Text: text})
	if err != nil {
		t.Fatalf("post %s/%s: %v", pane, key, err)
	}
	return a
}

func TestAskTextSanitizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"OK to deploy?\r\n\tthe site", "OK to deploy? the site"},
		{"  paste\x1b[2Jhere\x07  ", "paste[2Jhere"},
		// Bidi overrides would show the person text in another order.
		{"renew \u202etoken\u202c now", "renew token now"},
		// Zero-width space, word joiner, BOM, soft hyphen and tag characters are format characters.
		{"a\u200bb\u2060c\ufeffd\u00ade\U000E0041", "abcde"},
		// Joiners and variation selectors stay: emoji and scripts need them.
		{"👩\u200d💻 ok ❤\ufe0f न\u200dि", "👩\u200d💻 ok ❤\ufe0f न\u200dि"},
		{"line\u2028two", "line two"},
		{"\x00\x01\u200b", ""},
	} {
		if got := askText(tc.in); got != tc.want {
			t.Errorf("askText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := askText(strings.Repeat("é", 500))
	if n := len([]rune(long)); n != askTextMaxRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("long text: %d runes %q", n, long[len(long)-6:])
	}
}

func TestAskPostUpdatesInPlaceByKey(t *testing.T) {
	s, _ := newTestAskStore(t)
	first, created, err := s.post(askPost{Pane: "p1", Source: "@secure", Key: "Bot-Token", Text: "renew the bot token"})
	if err != nil || !created || first.Key != "bot-token" || first.Kind != askKindAgent {
		t.Fatalf("first post: %+v %v %v", first, created, err)
	}
	again, created, err := s.post(askPost{Pane: "p1", Key: "bot-token", Text: "renew the bot token by 18:00"})
	if err != nil || created || again.ID != first.ID || again.Text != "renew the bot token by 18:00" {
		t.Fatalf("update: %+v %v %v", again, created, err)
	}
	// Another pane's key of the same name is its own ask.
	other := mustPost(t, s, "p2", "bot-token", "renew it")
	if other.ID == first.ID {
		t.Fatal("two panes share an ask")
	}
	if n := len(s.openFor("p1")); n != 1 {
		t.Fatalf("p1 open = %d, want 1", n)
	}
	for _, bad := range []askPost{{Pane: "p1", Key: "", Text: "x"}, {Pane: "p1", Key: "has space", Text: "x"}, {Pane: "p1", Key: "k", Text: "\u200b"}, {Pane: "p1", Key: "k", Text: "x", ResolveOn: "never"}} {
		if _, _, err := s.post(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Updating an ask saves the list, which prunes old closed asks: the update
// must not read the ask back by its old position.
func TestAskUpdateAfterPrune(t *testing.T) {
	s, now := newTestAskStore(t)
	old := mustPost(t, s, "p1", "old", "closed long ago")
	mustPost(t, s, "p1", "keep", "still open")
	if err := s.resolve("p1", old.Key); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(askClosedKeep + time.Hour)
	got, created, err := s.post(askPost{Pane: "p1", Key: "keep", Text: "still open, now urgent"})
	if err != nil || created || got.Key != "keep" || got.Text != "still open, now urgent" {
		t.Fatalf("update after prune: %+v %v %v", got, created, err)
	}
	if list := s.list(); len(list) != 1 || list[0].Key != "keep" {
		t.Fatalf("list = %+v", list)
	}
	// The store is not left locked.
	mustPost(t, s, "p1", "next", "another")
}

func TestAskLimits(t *testing.T) {
	s, now := newTestAskStore(t)
	for i := 0; i < askOpenPerPane; i++ {
		mustPost(t, s, "p1", "k"+string(rune('a'+i)), "ask")
	}
	if _, _, err := s.post(askPost{Pane: "p1", Key: "kf", Text: "one too many"}); err == nil || !strings.Contains(err.Error(), "resolve one") {
		t.Fatalf("6th open ask: %v", err)
	}
	// Updating an open ask is still allowed at the open cap.
	if _, _, err := s.post(askPost{Pane: "p1", Key: "ka", Text: "changed"}); err != nil {
		t.Fatalf("update at cap: %v", err)
	}
	// New asks per hour: resolving makes room in the open cap but not the hourly one.
	if err := s.resolve("p1", "ka"); err != nil {
		t.Fatal(err)
	}
	mustPost(t, s, "p1", "kf", "sixth new this hour")
	if err := s.resolve("p1", "kb"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(askPost{Pane: "p1", Key: "kg", Text: "seventh"}); err == nil || !strings.Contains(err.Error(), "new asks in the last hour") {
		t.Fatalf("7th new ask in an hour: %v", err)
	}
	// Total posts per hour bound rewriting too.
	s2, _ := newTestAskStore(t)
	mustPost(t, s2, "p1", "k", "v0")
	for i := 1; i < askPostsPerHour; i++ {
		mustPost(t, s2, "p1", "k", "v"+string(rune('a'+i%26))+strings.Repeat("x", i))
	}
	if _, _, err := s2.post(askPost{Pane: "p1", Key: "k", Text: "one more"}); err == nil {
		t.Fatal("31st post in an hour accepted")
	}
	// An unchanged repeat changes nothing and costs nothing.
	if a, created, err := s2.post(askPost{Pane: "p1", Key: "k", Text: s2.openFor("p1")[0].Text}); err != nil || created {
		t.Fatalf("unchanged repeat: %+v %v %v", a, created, err)
	}
	// An hour later the window has moved on.
	*now = now.Add(time.Hour + time.Second)
	mustPost(t, s, "p1", "kg", "next hour")
}

func TestAskDismissIsRememberedForFourHours(t *testing.T) {
	s, now := newTestAskStore(t)
	a := mustPost(t, s, "p1", "deploy", "OK to deploy?")
	if err := s.dismiss(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.dismiss(a.ID); err != errAskClosed {
		t.Fatalf("second dismiss: %v", err)
	}
	if err := s.dismiss("ask_missing"); err != errAskNotFound {
		t.Fatalf("missing dismiss: %v", err)
	}
	if _, _, err := s.post(askPost{Pane: "p1", Key: "deploy", Text: "OK to deploy?"}); err == nil || !strings.Contains(err.Error(), "dismissed") {
		t.Fatalf("re-post after dismiss: %v", err)
	}
	// Changed text is a new question.
	changed := mustPost(t, s, "p1", "deploy", "OK to deploy now that tests pass?")
	if err := s.dismiss(changed.ID); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(askDismissMemory + time.Minute)
	mustPost(t, s, "p1", "deploy", "OK to deploy now that tests pass?")
	// A key the agent resolved itself may be posted again at once.
	r := mustPost(t, s, "p1", "restart", "restart your PC")
	if err := s.resolve("p1", "restart"); err != nil {
		t.Fatal(err)
	}
	if again := mustPost(t, s, "p1", "restart", "restart your PC"); again.ID == r.ID {
		t.Fatal("resolved ask reopened instead of a new one")
	}
}

func TestAskResolveOnlyOwn(t *testing.T) {
	s, _ := newTestAskStore(t)
	mustPost(t, s, "p1", "deploy", "OK to deploy?")
	if err := s.resolve("p2", "deploy"); err != errAskNotFound {
		t.Fatalf("another pane resolved p1's ask: %v", err)
	}
	if len(s.openFor("p1")) != 1 {
		t.Fatal("p1's ask closed")
	}
}

func TestAskAutoResolve(t *testing.T) {
	s, now := newTestAskStore(t)
	mustPost(t, s, "lead", "keep", "stays")
	if _, _, err := s.post(askPost{Pane: "lead", Key: "q", Text: "which option?", ResolveOn: askResolveOnReply}); err != nil {
		t.Fatal(err)
	}
	mustPost(t, s, "gone", "x", "pane will close")
	if _, _, err := s.post(askPost{Pane: "lead", Kind: askKindWorker, Key: "worker-w1", Text: "@w1 retained", WorkerPane: "w1"}); err != nil {
		t.Fatal(err)
	}
	// Input before the ask was posted doesn't answer it.
	s.personReplied("lead", now.Add(-time.Second))
	if len(s.openFor("lead")) != 3 {
		t.Fatal("earlier input closed the ask")
	}
	s.personReplied("other", now.Add(time.Second))
	s.personReplied("lead", now.Add(time.Second))
	open := s.openFor("lead")
	if len(open) != 2 || open[0].Key != "keep" {
		t.Fatalf("after reply: %+v", open)
	}
	s.paneGone("gone")
	s.paneGone("w1")
	if len(s.openFor("gone")) != 0 || len(s.openFor("lead")) != 1 {
		t.Fatalf("after panes gone: gone=%v lead=%v", s.openFor("gone"), s.openFor("lead"))
	}
	mustPost(t, s, "missing", "y", "pane closed while the daemon was down")
	s.closeMissing(func(id string) bool { return id != "missing" })
	by := map[string]string{}
	for _, a := range s.list() {
		if a.State != askOpen {
			by[a.Key] = a.ClosedBy
		}
	}
	if by["q"] != "replied" || by["x"] != "pane_closed" || by["worker-w1"] != "worker_gone" || by["y"] != "pane_closed" {
		t.Fatalf("closedBy = %v", by)
	}
}

func TestAskStorePersistsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := newAskStore(dir, nil)
	s.now = func() time.Time { return now }
	a := mustPost(t, s, "p1", "deploy", "OK to deploy?")
	b := mustPost(t, s, "p1", "old", "will be dismissed")
	if err := s.dismiss(b.ID); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "asks.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("asks.json: %v %v", info, err)
	}
	re := newAskStore(dir, nil)
	re.now = func() time.Time { return now.Add(askClosedKeep + time.Minute) }
	got := re.list()
	if len(got) != 1 || got[0].ID != a.ID || got[0].State != askOpen {
		t.Fatalf("reloaded = %+v", got)
	}
	// A broken file is set aside, not trusted.
	if err := os.WriteFile(filepath.Join(dir, "asks.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bad := newAskStore(dir, nil); len(bad.list()) != 0 {
		t.Fatal("corrupt file loaded")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "asks.json.corrupt-*")); len(m) != 1 {
		t.Fatalf("corrupt file not kept aside: %v", m)
	}
}

// Only a pane the daemon traced may post or resolve asks; an unverifiable
// header or environment claim may not, nor a daemon-started worker.
func TestAskToolsNeedATracedPane(t *testing.T) {
	a := traceTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	lead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}
	args := map[string]any{"key": "deploy", "text": "OK to deploy?"}
	for name, traced := range map[string]*callerIdentity{
		"untraceable header": nil,
		"environment claim":  {Kind: callerPane, PaneID: "lead", PID: 7, FromEnv: true},
		"daemon worker":      {Kind: callerWorker, PID: 7},
		"program outside":    {Kind: callerUnverified, PID: 7},
	} {
		if text, failed := mcpAs(t, a, traced, "lead", "post_user_ask", args); !failed || !strings.Contains(text, "couldn't confirm which pane") {
			t.Errorf("%s posted: %q", name, text)
		}
	}
	if len(a.asks.list()) != 0 {
		t.Fatal("an unverified caller posted an ask")
	}
	text, failed := mcpAs(t, a, lead, "rogue", "post_user_ask", args)
	if failed {
		t.Fatalf("traced pane: %q", text)
	}
	var res struct {
		Status string        `json:"status"`
		Open   []askToolOpen `json:"open_asks"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || res.Status != "posted" || len(res.Open) != 1 {
		t.Fatalf("result %q: %v", text, err)
	}
	// The ask is the traced pane's, whatever the header said, with the
	// daemon's label for it.
	got := a.asks.list()
	if len(got) != 1 || got[0].SourcePane != "lead" || got[0].Source == "" {
		t.Fatalf("asks = %+v", got)
	}
	// Another pane can't resolve it, even naming the lead in its header.
	rogue := &callerIdentity{Kind: callerPane, PaneID: "rogue", PID: 8}
	if text, failed := mcpAs(t, a, rogue, "lead", "resolve_user_ask", map[string]any{"key": "deploy"}); !failed {
		t.Fatalf("rogue resolved lead's ask: %q", text)
	}
	if text, failed := mcpAs(t, a, nil, "lead", "resolve_user_ask", map[string]any{"key": "deploy"}); !failed {
		t.Fatalf("untraceable caller resolved: %q", text)
	}
	// list_panes counts it.
	text, _ = mcpAs(t, a, rogue, "", "list_panes", map[string]any{})
	if !strings.Contains(text, `"open_asks":1`) {
		t.Fatalf("list_panes has no open_asks: %s", text)
	}
	if text, failed := mcpAs(t, a, lead, "", "resolve_user_ask", map[string]any{"key": "deploy"}); failed {
		t.Fatalf("own resolve: %q", text)
	}
	if len(a.asks.openFor("lead")) != 0 {
		t.Fatal("ask still open")
	}
}

// Only the person dismisses; agent authority does not extend to it.
func TestAskDismissNeedsThePerson(t *testing.T) {
	a := confirmTestServer(t)
	a.agentAuthority = newAgentAuthority(t.TempDir())
	a.asks = newAskStore(t.TempDir(), nil)
	ask := mustPost(t, a.asks, "lead", "deploy", "OK to deploy?")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/asks", a.listAsks)
	mux.HandleFunc("POST /api/asks/{id}/dismiss", a.dismissAsk)
	do := func(who callerKind, method, path string) *httptest.ResponseRecorder {
		prev := identifyCallerHook
		identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: who, PaneID: "lead"} }
		defer func() { identifyCallerHook = prev }()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	for _, who := range []callerKind{callerPane, callerWorker, callerUnverified} {
		if rec := do(who, "POST", "/api/asks/"+ask.ID+"/dismiss"); rec.Code == http.StatusOK {
			t.Fatalf("%s dismissed an ask", who)
		}
	}
	rec := do(callerHuman, "GET", "/api/asks")
	var list struct {
		Asks []askView `json:"asks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Asks) != 1 || !list.Asks[0].Open || list.Asks[0].Source == "" {
		t.Fatalf("GET /api/asks: %s", rec.Body.String())
	}
	if rec := do(callerHuman, "POST", "/api/asks/"+ask.ID+"/dismiss"); rec.Code != http.StatusOK {
		t.Fatalf("person's dismiss: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(callerHuman, "POST", "/api/asks/"+ask.ID+"/dismiss"); rec.Code != http.StatusConflict {
		t.Fatalf("second dismiss: %d", rec.Code)
	}
}

// A resolve_on "reply" ask closes on the person's Enter in a human socket,
// never on an agent's keystrokes into the pane.
func TestAskReplyCountsOnlyThePersonsEnter(t *testing.T) {
	h := newAdoptTestPane(t, false)
	submits := make(chan string, 8)
	h.m.SetOnHumanSubmit(func(id string, _ time.Time) { submits <- id })

	agent := h.dial(t, callerPane, "")
	agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	agent.expectOwnership(t, true, "you")
	agent.send(t, websocket.BinaryMessage, "yes\r")
	h.expectWrite(t, "yes\r")
	agent.send(t, websocket.TextMessage, `{"type":"input","data":"ok\r"}`)
	h.expectWrite(t, "ok\r")
	select {
	case id := <-submits:
		t.Fatalf("agent input counted as the person's reply in %s", id)
	case <-time.After(200 * time.Millisecond):
	}
	_ = agent.conn.Close()

	h2 := newAdoptTestPane(t, false)
	submits2 := make(chan string, 8)
	h2.m.SetOnHumanSubmit(func(id string, _ time.Time) { submits2 <- id })
	person := h2.dial(t, callerHuman, "")
	person.send(t, websocket.BinaryMessage, "ye")
	h2.expectWrite(t, "ye")
	// A paste with a newline inside is not a submit.
	person.send(t, websocket.BinaryMessage, "\x1b[200~a\rb\x1b[201~")
	h2.expectWrite(t, "\x1b[200~a\rb\x1b[201~")
	select {
	case <-submits2:
		t.Fatal("typing without Enter counted as a reply")
	case <-time.After(200 * time.Millisecond):
	}
	person.send(t, websocket.BinaryMessage, "\r")
	h2.expectWrite(t, "\r")
	select {
	case id := <-submits2:
		if id != "p1" {
			t.Fatalf("submit in %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the person's Enter was not seen")
	}
	person.send(t, websocket.TextMessage, `{"type":"input","data":"next\r"}`)
	h2.expectWrite(t, "next\r")
	select {
	case <-submits2:
	case <-time.After(2 * time.Second):
		t.Fatal("the composer's text plus Enter was not seen")
	}
}
