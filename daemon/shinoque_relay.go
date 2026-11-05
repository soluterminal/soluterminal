package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const shinoqueUpdateRequired = "Update Shinoque to use it remotely."

const shinoqueUnavailable = "shinoque is not enabled for the relay"

var shinoqueCloudPrefix = regexp.MustCompile(`^/m/[A-Za-z0-9_-]+(/b/[A-Za-z0-9_-]+)?$`)
var shinoquePrivateDevicePattern = regexp.MustCompile(`^dev_[A-Za-z0-9_-]{22}$`)

// Protocol shared with Shinoque's apps/core/src/http/relay.ts privateDeviceProof.
// The key is the literal token string, not its hex-decoded bytes.
func shinoquePrivateDeviceProof(token, device string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("shinoque-private-device-v1|" + device))
	return hex.EncodeToString(mac.Sum(nil))
}

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
	info      os.FileInfo
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

// Revalidate metadata on every request, including cache hits. Installed Linux
// tokens have the same ownership, mode and no-symlink requirements as discovery.
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
	c.mu.Lock()
	defer c.mu.Unlock()
	// A changed or lost token file invalidates the solud verdict; the
	// generation counter never sees the token itself.
	fail := func() string {
		if c.path != "" {
			c.discovery.invalidate()
		}
		c.path, c.token, c.info = "", "", nil
		return ""
	}
	info, err := statShinoqueToken(path)
	if err != nil || !shinoqueTokenMetadata(info) {
		return fail()
	}
	if c.path == path && sameShinoqueTokenFile(c.info, info) {
		return c.token
	}
	file, err := openShinoqueToken(path)
	if err != nil {
		return fail()
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !shinoqueTokenMetadata(opened) || !sameShinoqueTokenFile(info, opened) {
		return fail()
	}
	content, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(content) > 4096 {
		return fail()
	}
	after, err := file.Stat()
	if err != nil || !shinoqueTokenMetadata(after) || !sameShinoqueTokenFile(opened, after) {
		return fail()
	}
	if c.path != "" {
		c.discovery.invalidate()
	}
	c.path, c.info = path, after
	c.token = strings.TrimSpace(string(content))
	return c.token
}

func sameShinoqueTokenFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
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

// Shinoque's checkRelayRequest uses browser headers to reject cross-site API
// calls and upgrades, but permits top-level page navigation. Preserve that
// check here, before removing headers that could confer authority on a solud
// at a forged destination. The outgoing token and prefix authenticate relay use.
func shinoqueRelayBrowserAllowed(r *http.Request, relayURL string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		path := strings.TrimPrefix(r.URL.Path, "/shinoque")
		dest := r.Header.Get("Sec-Fetch-Dest")
		navigation := (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			path != "/api" && !strings.HasPrefix(path, "/api/") && r.Header.Get("Upgrade") == "" &&
			r.Header.Get("Sec-Fetch-Mode") == "navigate" && (dest == "" || dest == "document")
		if !navigation {
			return false
		}
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 || origins[0] == "" {
		return false
	}
	origin := origins[0]
	// Both readable origins come exclusively from daemon-owned configuration.
	// Private documents have a separate origin, never either readable origin.
	matched := false
	if privateRelayDevice(r) != "" {
		matched = origin == "https://private.shinoque.invalid"
	} else {
		appOrigin := shinoqueWebOrigin(publicAppURL, false)
		servingOrigin := shinoqueRelayWebOrigin(relayURL)
		matched = (appOrigin != "" && origin == appOrigin) || (servingOrigin != "" && origin == servingOrigin)
	}
	if !matched || origin != shinoqueWebOrigin(origin, true) {
		return false
	}
	if referers := r.Header.Values("Referer"); len(referers) != 0 {
		if len(referers) != 1 || shinoqueWebOrigin(referers[0], true) != origin {
			return false
		}
	}
	return true
}

// shinoqueWebOrigin normalizes configured URLs and Referers to a web origin.
// HTTP is reserved for an explicitly allowed ws loopback relay; a public app
// setting alone cannot authorize an insecure origin. Requests must equal the
// resulting canonical origin exactly (lowercase host, no default port/path).
func shinoqueWebOrigin(raw string, allowLoopbackHTTP bool) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") {
		return ""
	}
	if u.Scheme != "https" && !(allowLoopbackHTTP && u.Scheme == "http" &&
		(host == "127.0.0.1" || host == "::1" || host == "localhost")) {
		return ""
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
		port = strconv.Itoa(n)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host
}

func shinoqueRelayWebOrigin(relayURL string) string {
	u, err := url.Parse(relayURL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return ""
	}
	allowHTTP := u.Scheme == "ws"
	u.Scheme = "https"
	if allowHTTP {
		u.Scheme = "http"
	}
	return shinoqueWebOrigin(u.String(), allowHTTP)
}

// The relay's upstream transport is http.DefaultTransport with at most 8 dials
// in flight, so a cold-launch burst does not connect all at once; some WSL
// network modes (consomme) refuse wider loopback connect bursts. Only dials are
// limited: open streaming responses never hold a slot.
var shinoqueRelayTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = shinoqueLimitDials(transport.DialContext, 8)
	return transport
}()

type shinoqueDialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// shinoqueLimitDials holds a slot only while dial runs: a waiter gives up when
// its context ends, and every return from dial releases the slot.
func shinoqueLimitDials(dial shinoqueDialFunc, limit int) shinoqueDialFunc {
	slots := make(chan struct{}, limit)
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-slots }()
		return dial(ctx, network, address)
	}
}

// This handler is installed only on the authenticated cloud tunnel. It has no
// HTML rewriting or CSP mutation: shinoque owns its document sandbox.
func newShinoqueRelayHandler(next http.Handler, tokens *shinoqueTokenCache, activeRelayURL func() string) http.Handler {
	return newShinoqueRelayHandlerWithTables(next, tokens, activeRelayURL, shinoqueLocalListenerAllowedFrom)
}

func newShinoqueRelayHandlerWithListenerCheck(next http.Handler, tokens *shinoqueTokenCache, activeRelayURL func() string, listenerAllowed func(string) bool) http.Handler {
	return newShinoqueRelayHandlerWithTables(next, tokens, activeRelayURL, func(port string, _ *shinoqueProcTables) bool {
		return listenerAllowed(port)
	})
}

func newShinoqueRelayHandlerWithTables(next http.Handler, tokens *shinoqueTokenCache, activeRelayURL func() string, listenerAllowed func(string, *shinoqueProcTables) bool) http.Handler {
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
		if device := privateRelayDevice(r); device != "" && !shinoquePrivateDevicePattern.MatchString(device) {
			http.Error(w, "invalid private relay device", http.StatusForbidden)
			return
		}
		relayURL := ""
		if activeRelayURL != nil {
			relayURL = activeRelayURL()
		}
		if !shinoqueRelayBrowserAllowed(r, relayURL) {
			http.Error(w, "invalid relay origin", http.StatusForbidden)
			return
		}
		tables := &shinoqueProcTables{}
		config, ok := tokens.discovery.resolveWith(tables)
		if !ok {
			http.Error(w, shinoqueUnavailable, http.StatusServiceUnavailable)
			return
		}
		// Only proxy when the local Shinoque advertises strict relay auth.
		if !config.supportsRelayAuth() || (privateRelayDevice(r) != "" && !config.supportsPrivateDeviceProof()) {
			http.Error(w, shinoqueUpdateRequired, http.StatusServiceUnavailable)
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
		// Resolve discovery for this request, then check and proxy the same
		// immutable port. Do not re-resolve the destination after this check.
		if !listenerAllowed(port, tables) {
			http.Error(w, shinoqueUnavailable, http.StatusServiceUnavailable)
			return
		}
		target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
		rest := strings.TrimPrefix(r.URL.Path, "/shinoque")
		if rest == "" {
			rest = "/"
		}
		proxy := &httputil.ReverseProxy{
			Transport:     shinoqueRelayTransport,
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
					lower := strings.ToLower(name)
					if strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "sec-fetch-") {
						pr.Out.Header.Del(name)
					}
				}
				pr.Out.Header.Del("Forwarded")
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("Proxy-Authorization")
				pr.Out.Header.Del("Cookie")
				// A forged destination may be another solud. Never let the
				// proxy's local process plus browser headers confer human authority.
				// Shinoque authenticates with relay credentials and the prefix.
				pr.Out.Header.Del("Origin")
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix+"/shinoque")
				// Only the authenticated private channel may identify a device.
				// The readable relay must never inherit a client-supplied claim.
				pr.Out.Header.Del("X-Shinoque-Private-Device")
				if device := privateRelayDevice(pr.In); device != "" {
					pr.Out.Header.Set("X-Shinoque-Private-Device", device)
					pr.Out.Header.Set("X-Shinoque-Relay", shinoquePrivateDeviceProof(token, device))
				} else {
					pr.Out.Header.Set("X-Shinoque-Relay", token)
				}
			},
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Del("Set-Cookie")
				return nil
			},
			// A refused, failed or cut upstream connection may mean the
			// listener changed: rescan before the next forward. The log and
			// 502 are ReverseProxy's defaults.
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				// The client's own abort says nothing about the listener; every
				// other failure, deadlines included, forces a rescan.
				if !errors.Is(err, context.Canceled) || r.Context().Err() == nil {
					tokens.discovery.invalidate()
				}
				log.Printf("http: proxy error: %v", err)
				w.WriteHeader(http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}
