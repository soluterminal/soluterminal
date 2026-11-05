package main

import (
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

var requestMediaCounts sync.Map

// This rollout observes compatibility before enforcing media types. Only
// registered route patterns and fixed client labels enter the counters/logs;
// URLs, bodies, credentials and arbitrary user-agent values are never logged.
func observeRequestMedia(r *http.Request, pattern string) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return
	}
	path := appPolicyPath(r.URL.Path)
	if strings.HasPrefix(path, "/api/proxy/") || strings.HasPrefix(path, "/api/preview-serve/") || path == "/api/transcribe" {
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		if r.Header.Get("Content-Type") != "" || r.ContentLength == 0 {
			return
		}
		kind = "missing"
	}
	switch kind {
	case "text/plain", "application/x-www-form-urlencoded", "multipart/form-data", "missing":
	default:
		return
	}
	if pattern == "" {
		pattern = "unmatched"
	}
	client := "program"
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != "" {
		client = "browser"
	}
	if requestViaRelay(r) {
		client = "relay"
	}
	value, _ := requestMediaCounts.LoadOrStore(pattern+"|"+client, &atomic.Uint64{})
	count := value.(*atomic.Uint64).Add(1)
	// Retain counts while bounding repetitive log output.
	if count&(count-1) == 0 {
		log.Printf("request-media route=%q client=%s type=%s count=%d mode=observe", pattern, client, kind, count)
	}
}

func requestMediaObserver(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := ""
		if mux, ok := next.(*http.ServeMux); ok {
			_, pattern = mux.Handler(r)
		}
		observeRequestMedia(r, pattern)
		next.ServeHTTP(w, r)
	})
}
