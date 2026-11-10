package main

import (
	"strings"
	"testing"
	"time"
)

func openBrowserTestAria(t *testing.T) *Aria {
	t.Helper()
	pr := &PortRegistry{seen: map[string]*Port{
		"tcp|127.0.0.1|5173": {
			Port: 5173, Addr: "127.0.0.1", Proto: "tcp", PaneID: "pane_1", WebStatus: "ready",
			PreviewPath: "/api/proxy/5173~c2VsNHY/", FirstAt: time.Unix(10, 0),
		},
		"tcp6|::1|5173": {
			Port: 5173, Addr: "::1", Proto: "tcp6", PaneID: "pane_2", WebStatus: "ready",
			PreviewPath: "/api/proxy/5173~c2VsNnY/", FirstAt: time.Unix(10, 0),
		},
		"tcp|0.0.0.0|8443": {
			Port: 8443, Addr: "0.0.0.0", Proto: "tcp", PaneID: "pane_1", WebStatus: "ready",
			Scheme: "https", PreviewPath: "/api/proxy/8443~c2VsdGxz/", FirstAt: time.Unix(10, 0),
		},
		// Not a pane's: discovered, but outside SoluTerminal.
		"tcp|127.0.0.1|5432": {
			Port: 5432, Addr: "127.0.0.1", Proto: "tcp", WebStatus: "ready",
			PreviewPath: "/api/proxy/5432~c2VscGc/", FirstAt: time.Unix(10, 0),
		},
		// A pane's, but not a web service.
		"tcp|127.0.0.1|9229": {
			Port: 9229, Addr: "127.0.0.1", Proto: "tcp", PaneID: "pane_1", WebStatus: "internal",
			PreviewPath: "/api/proxy/9229~c2VsZGJn/", FirstAt: time.Unix(10, 0),
		},
	}}
	return &Aria{ports: pr, uiBus: newUIBus()}
}

func TestOpenBrowserOpensListedPaneServices(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url, opened, pane string
	}{
		{"localhost url", "http://localhost:5173", "http://localhost:5173", "pane_1"},
		{"loopback ip with path and query", "http://127.0.0.1:5173/admin?tab=2#top", "http://localhost:5173/admin?tab=2#top", "pane_1"},
		{"ipv6 loopback prefers the ipv6 listener", "http://[::1]:5173/", "http://[::1]:5173/", "pane_2"},
		{"https service", "https://localhost:8443/x", "https://127.0.0.1:8443/x", "pane_1"},
		{"preview path", "/api/proxy/5173~c2VsNnY/docs", "http://[::1]:5173/docs", "pane_2"},
		{"preview path behind a relay prefix", "https://cloud.example/m/abc/api/proxy/5173~c2VsNHY/", "http://localhost:5173/", "pane_1"},
		{"drops a sign-in token", "http://localhost:5173/?access_token=secret&q=1", "http://localhost:5173/?q=1", "pane_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := openBrowserTestAria(t)
			res := a.toolOpenBrowser(tc.url)
			if strings.HasPrefix(res, "error:") {
				t.Fatalf("toolOpenBrowser(%q) = %s", tc.url, res)
			}
			events := a.uiBus.Recent()
			if len(events) != 1 || events[0].Type != "open-browser" {
				t.Fatalf("events = %#v, want one open-browser", events)
			}
			e := events[0]
			if e.URL != tc.opened {
				t.Fatalf("URL = %q, want %q", e.URL, tc.opened)
			}
			if e.Port == nil || e.Port.PaneID != tc.pane {
				t.Fatalf("event port = %#v, want pane %s", e.Port, tc.pane)
			}
			if strings.Contains(res, "secret") || strings.Contains(e.URL, "secret") {
				t.Fatalf("token carried along: %s / %s", res, e.URL)
			}
		})
	}
}

func TestOpenBrowserRefusesAnythingButAListedService(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://example.com",
		"http://localhost:3000",           // nothing listens there
		"http://localhost:5432",           // not a pane's service
		"http://localhost:9229",           // a pane's, but not a web service
		"/api/proxy/5432~c2VscGc/",        // preview path of an unowned listener
		"/api/proxy/5173~forged/",         // unknown selector
		"/api/proxy/5173/",                // numeric path without a listed selector
		"https://localhost:5173",          // wrong scheme for the service
		"http://192.168.1.20:5173",        // not loopback, not the listener's address
		"http://user:pass@localhost:5173", // credentials in the URL
		"file:///etc/passwd",
		"javascript:alert(1)",
		"",
	} {
		a := openBrowserTestAria(t)
		res := a.toolOpenBrowser(raw)
		if !strings.HasPrefix(res, "error:") {
			t.Fatalf("toolOpenBrowser(%q) = %s, want a refusal", raw, res)
		}
		if !strings.Contains(res, "list_ports") || !strings.Contains(res, "system browser") {
			t.Fatalf("refusal for %q does not point at list_ports and the system browser: %s", raw, res)
		}
		if n := len(a.uiBus.Recent()); n != 0 {
			t.Fatalf("toolOpenBrowser(%q) published %d events", raw, n)
		}
	}
}

func TestMCPOpenBrowserRefusalIsAToolError(t *testing.T) {
	t.Parallel()
	a := &apiServer{aria: openBrowserTestAria(t)}
	res, ok := a.mcpToolOpenBrowser(map[string]any{"url": "https://example.com"}).(map[string]any)
	if !ok || res["isError"] != true {
		t.Fatalf("result = %#v, want isError", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "system browser") || strings.HasPrefix(text, "error:") {
		t.Fatalf("text = %q", text)
	}
	if n := len(a.aria.uiBus.Recent()); n != 0 {
		t.Fatalf("published %d events", n)
	}
}
