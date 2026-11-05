package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Operator additions are exact origins, separated by spaces or commas.
// The same list controls navigation and the document's ancestor policy.
func embedParents() []string {
	parents := []string{
		"http://127.0.0.1:14700", "http://127.0.0.1:4700", "http://127.0.0.1:4760",
		"http://127.0.0.1:47800", "http://127.0.0.1:46800", "http://127.0.0.1:45800", "http://127.0.0.1:44800",
		"http://127.0.0.1:43800", "http://127.0.0.1:42800", "http://127.0.0.1:41800", "http://127.0.0.1:40800",
		"http://window.shinoque.localhost:47800", "http://window.shinoque.localhost:46800",
		"http://window.shinoque.localhost:45800", "http://window.shinoque.localhost:44800",
		"http://window.shinoque.localhost:43800", "http://window.shinoque.localhost:42800",
		"http://window.shinoque.localhost:41800", "http://window.shinoque.localhost:40800",
	}
	for _, raw := range strings.FieldsFunc(os.Getenv("SOLU_EMBED_PARENTS"), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if origin := loopbackEmbedOrigin(raw); origin != "" {
			parents = append(parents, origin)
		}
	}
	return parents
}

func loopbackEmbedOrigin(raw string) string {
	origin := exactWebOrigin(raw)
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || !safeLoopbackHost(u.Host) {
		return ""
	}
	return origin
}

func exactWebOrigin(raw string) string {
	if strings.ContainsAny(raw, "*;'\"\\\r\n\t ") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Policy follows the selected backend without changing routing itself.
func appPolicyPath(path string) string {
	for _, prefix := range []string{"/b/wsl", "/b/win"} {
		if strings.HasPrefix(path, prefix+"/") {
			return strings.TrimPrefix(path, prefix)
		}
	}
	return path
}

func sameSiteNavigation(r *http.Request) bool {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Upgrade") != "" ||
		r.Header.Get("Sec-Fetch-Mode") != "navigate" {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "same-site" || site == "same-origin"
}

func frameNavigationShape(r *http.Request) bool {
	dest := r.Header.Get("Sec-Fetch-Dest")
	return sameSiteNavigation(r) && (dest == "iframe" || dest == "frame")
}

func previewNavigationShape(r *http.Request) bool {
	dest := r.Header.Get("Sec-Fetch-Dest")
	return sameSiteNavigation(r) && (dest == "document" || dest == "iframe" || dest == "frame")
}

func permittedAppFrame(r *http.Request) bool {
	path := appPolicyPath(r.URL.Path)
	if !frameNavigationShape(r) || path == "/api" || strings.HasPrefix(path, "/api/") ||
		path == "/debug" || strings.HasPrefix(path, "/debug/") {
		return false
	}
	for _, header := range []string{"Origin", "Referer"} {
		if len(r.Header.Values(header)) > 1 {
			return false
		}
		if raw := r.Header.Get(header); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || u.User != nil || u.Host == "" {
				return false
			}
			origin := exactWebOrigin(u.Scheme + "://" + u.Host)
			allowed := origin != "" && strings.EqualFold(u.Host, r.Host)
			for _, parent := range embedParents() {
				allowed = allowed || origin == parent
			}
			if !allowed {
				return false
			}
		}
	}
	return true
}

func appDocumentHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := appPolicyPath(r.URL.Path)
		// User-service responses keep their own proxy sandbox policies. They
		// are not app documents, including on the router's preview listener.
		if !strings.HasPrefix(path, "/api/proxy/") && !strings.HasPrefix(path, "/api/preview-serve/") {
			w.Header().Add("Content-Security-Policy", "frame-ancestors 'self' "+strings.Join(embedParents(), " "))
		}
		next.ServeHTTP(w, r)
	})
}
