package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestDiagnosticAccessThroughOldRouter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &apiServer{gate: &authGate{token: "test-backend"}}
	var reads atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", a.diagnosticAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})))
	backend := httptest.NewServer(a.gate.middleware(a.apiHandler(mux)))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, prefix := range []string{"", "/b/wsl", "/b/win"} {
		frontend := httptest.NewServer(oldRouterTestProxy(target, prefix, "test-backend"))
		for _, method := range []string{"GET", "HEAD", "POST"} {
			for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/pprof/trace"} {
				for _, connection := range []string{"", "X-Forwarded-For"} {
					r, _ := http.NewRequest(method, frontend.URL+prefix+path, nil)
					r.Header.Set("Connection", connection)
					r.Header.Set("X-Forwarded-For", "")
					resp, err := http.DefaultClient.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusForbidden {
						t.Fatalf("%s %s%s connection=%q status=%d", method, prefix, path, connection, resp.StatusCode)
					}
				}
			}
		}
		frontend.Close()
	}
	if reads.Load() != 0 {
		t.Fatal("old router reached a diagnostic handler")
	}
	r, _ := http.NewRequest(http.MethodGet, backend.URL+"/debug/pprof/heap", nil)
	r.Header.Set("Authorization", "Bearer test-backend")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || reads.Load() != 1 {
		t.Fatal("direct owner connection lost diagnostic access")
	}
}

func TestDiagnosticAccessOnEveryTransport(t *testing.T) {
	previous := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = previous })
	for _, transport := range []string{"local", "relay", "private"} {
		for _, credential := range []string{"none", "bearer", "cookie", "person"} {
			for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/profile", "/debug/pprof/trace", "/debug/pprof/heap"} {
				t.Run(transport+"/"+credential+path, func(t *testing.T) {
					identifyCallerHook = func(*http.Request) callerIdentity {
						if credential == "person" {
							return callerIdentity{Kind: callerHuman, Relay: transport != "local"}
						}
						return callerIdentity{Kind: callerUnverified}
					}
					a := &apiServer{gate: &authGate{token: "test-master"}}
					reached := false
					var h http.Handler = a.diagnosticAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						reached = true
						w.WriteHeader(http.StatusNoContent)
					}))
					if transport != "local" {
						h = markRelayTransport(h)
					}
					r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:41001"+path, nil)
					r.RemoteAddr = "127.0.0.1:41003"
					if credential == "bearer" {
						r.Header.Set("Authorization", "Bearer test-master")
					}
					if credential == "cookie" {
						r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "test-master"})
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if reached != (transport == "local" && credential != "none") {
						t.Fatalf("reached=%v status=%d", reached, w.Code)
					}
					if transport != "local" && w.Code != http.StatusForbidden {
						t.Fatalf("remote diagnostics status=%d", w.Code)
					}
					// Even navigations with an owner credential keep the
					// diagnostic origin check; the stub never profiles anything.
					reached = false
					r.Header.Set("Origin", "http://127.0.0.1:41002")
					r.Header.Set("Sec-Fetch-Mode", "navigate")
					r.Header.Set("Sec-Fetch-Dest", "document")
					w = httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if reached || w.Code != http.StatusForbidden {
						t.Fatalf("foreign navigation reached=%v status=%d", reached, w.Code)
					}
				})
			}
		}
	}
}
