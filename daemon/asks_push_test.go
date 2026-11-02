package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A burst of new asks becomes one push naming who is waiting (the daemon's
// labels) and, for a single ask, the ask itself; closed asks drop out.
func TestAskPushTextCombinesABurst(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	x := mustPost(t, a.asks, "lead", "deploy", "OK to deploy?")
	if title, summary, pane := a.askPushText([]string{x.ID}); title == "" || summary != "OK to deploy?" || pane != "lead" {
		t.Fatalf("one ask: %q %q %q", title, summary, pane)
	}
	y := mustPost(t, a.asks, "social", "token", "renew the token")
	z := mustPost(t, a.asks, "lead", "restart", "restart your PC")
	title, summary, _ := a.askPushText([]string{x.ID, y.ID, z.ID})
	if summary != "3 new asks" || strings.Count(title, ",") != 1 {
		t.Fatalf("burst: %q %q", title, summary)
	}
	if err := a.asks.dismiss(y.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.asks.resolve("lead", "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, summary, pane := a.askPushText([]string{x.ID, y.ID, z.ID}); summary != "restart your PC" || pane != "lead" {
		t.Fatalf("after closes: %q %q", summary, pane)
	}
	if err := a.asks.resolve("lead", "restart"); err != nil {
		t.Fatal(err)
	}
	if _, _, pane := a.askPushText([]string{x.ID, y.ID, z.ID}); pane != "" {
		t.Fatal("pushed asks that are all closed")
	}
}

// Asks posted while a push is being delivered wait out the gap after it,
// instead of going out one window later.
func TestAskPushKeepsItsGapAcrossADelivery(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	a.asks.onCreated = a.queueAskPush
	const gap = 400 * time.Millisecond
	a.askPush.window, a.askPush.gap = 10*time.Millisecond, gap
	delivering := make(chan struct{})
	release := make(chan struct{})
	fired := make(chan time.Time, 4)
	first := true
	a.askPush.fire = func(pane, title, summary string) bool {
		if first {
			first = false
			close(delivering)
			<-release
		}
		fired <- time.Now()
		return true
	}
	mustPost(t, a.asks, "lead", "one", "first")
	<-delivering
	mustPost(t, a.asks, "lead", "two", "posted while the first push is in flight")
	close(release)
	t1 := <-fired
	select {
	case t2 := <-fired:
		if d := t2.Sub(t1); d < gap-50*time.Millisecond {
			t.Fatalf("second push %v after the first, want ≥ %v", d, gap)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the ask posted during delivery was never pushed")
	}
}

// The push goes out as "user-ask"; with only approved devices allowed, no
// ask text or pane label leaves the machine.
func TestFireAsksKeepsTextOffTheWireWhenPrivate(t *testing.T) {
	bodies := make(chan map[string]string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		bodies <- m
	}))
	t.Cleanup(srv.Close)
	private := false
	p := &Pusher{cloudBase: srv.URL, token: "tok", client: srv.Client(), lastFired: map[rateKey]time.Time{}, private: func() bool { return private }}
	if !p.FireAsks("lead", "@secure", "paste your token into #3") {
		t.Fatal("not delivered")
	}
	got := <-bodies
	if got["state"] != "user-ask" || got["paneId"] != "lead" || got["summary"] != "paste your token into #3" || got["title"] != "@secure" {
		t.Fatalf("push body = %v", got)
	}
	private = true
	p.FireAsks("lead", "@secure", "paste your token into #3")
	got = <-bodies
	if strings.Contains(got["summary"], "token") || strings.Contains(got["title"], "secure") || got["state"] != "user-ask" {
		t.Fatalf("private push leaked: %v", got)
	}
}

// /api/panes carries each pane's open asks with the daemon's label.
func TestListPanesCarriesOpenAsks(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	mustPost(t, a.asks, "lead", "deploy", "OK to deploy?")
	rec := httptest.NewRecorder()
	a.listPanes(rec, httptest.NewRequest("GET", "/api/panes", nil))
	var panes []struct {
		ID   string       `json:"id"`
		Asks []paneAskDTO `json:"asks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &panes); err != nil {
		t.Fatal(err)
	}
	for _, p := range panes {
		switch p.ID {
		case "lead":
			if len(p.Asks) != 1 || p.Asks[0].Text != "OK to deploy?" || p.Asks[0].Source == "" {
				t.Fatalf("lead asks = %+v", p.Asks)
			}
		default:
			if len(p.Asks) != 0 {
				t.Fatalf("%s has asks %+v", p.ID, p.Asks)
			}
		}
	}
}
