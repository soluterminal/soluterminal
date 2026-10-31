package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const shinoqueUnavailable = "shinoque is not enabled for the relay"

var shinoqueCloudPrefix = regexp.MustCompile(`^/m/[A-Za-z0-9_-]+(/b/[A-Za-z0-9_-]+)?$`)

func isShinoquePath(path string) bool {
	return path == "/shinoque" || strings.HasPrefix(path, "/shinoque/")
}

// The local listener never exposes this route, including to an authenticated
// local caller. The tunnel has a separate handler in main.go.
func denyLocalShinoque(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isShinoquePath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type shinoqueTokenCache struct {
	discovery shinoqueDiscoveryCache
	mu        sync.Mutex
	path      string
	mtime     time.Time
	size      int64
	token     string
}

func shinoqueTokenHome() string {
	home := os.Getenv("SHINOQUE_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".shinoque")
	}
	return home
}

// Stat on every request so token rotation is picked up without restarting
// solud. Cache the small file contents only while its mtime and size agree.
func (c *shinoqueTokenCache) read() string {
	config, ok := c.discovery.resolve()
	if !ok {
		return ""
	}
	return c.readHome(config.home)
}

func (c *shinoqueTokenCache) readHome(home string) string {
	if home == "" {
		return ""
	}
	path := filepath.Join(home, "relay-token")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		c.mu.Lock()
		c.path, c.token = "", ""
		c.mu.Unlock()
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == path && c.mtime.Equal(info.ModTime()) && c.size == info.Size() {
		return c.token
	}
	content, err := os.ReadFile(path)
	if err != nil {
		c.path, c.token = "", ""
		return ""
	}
	c.path, c.mtime, c.size = path, info.ModTime(), info.Size()
	c.token = strings.TrimSpace(string(content))
	return c.token
}

func shinoquePort() string {
	port := os.Getenv("SHINOQUE_RELAY_PORT")
	if port == "" {
		return "4760"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return ""
	}
	return strconv.Itoa(n)
}

// This handler is installed only on the authenticated cloud tunnel. It has no
// HTML rewriting or CSP mutation: shinoque owns its document sandbox.
func newShinoqueRelayHandler(next http.Handler, tokens *shinoqueTokenCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isShinoquePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		prefix := r.Header.Get("X-Forwarded-Prefix")
		if !shinoqueCloudPrefix.MatchString(prefix) {
			http.Error(w, "invalid relay prefix", http.StatusBadRequest)
			return
		}
		config, ok := tokens.discovery.resolve()
		if !ok {
			http.Error(w, shinoqueUnavailable, http.StatusServiceUnavailable)
			return
		}
		token := tokens.readHome(config.home)
		if token == "" {
			http.Error(w, shinoqueUnavailable, http.StatusServiceUnavailable)
			return
		}
		port := config.port
		if port == "" {
			http.Error(w, "invalid shinoque relay port", http.StatusServiceUnavailable)
			return
		}
		target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
		rest := strings.TrimPrefix(r.URL.Path, "/shinoque")
		if rest == "" {
			rest = "/"
		}
		proxy := &httputil.ReverseProxy{
			FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				pr.Out.URL.Path = rest
				pr.Out.URL.RawPath = ""
				query := pr.Out.URL.Query()
				if query.Has("access_token") {
					query.Del("access_token")
					pr.Out.URL.RawQuery = query.Encode()
				}
				for name := range pr.Out.Header {
					if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
						pr.Out.Header.Del(name)
					}
				}
				pr.Out.Header.Del("Forwarded")
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("Proxy-Authorization")
				pr.Out.Header.Del("Cookie")
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix+"/shinoque")
				pr.Out.Header.Set("X-Shinoque-Relay", token)
			},
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Del("Set-Cookie")
				return nil
			},
		}
		proxy.ServeHTTP(w, r)
	})
}
