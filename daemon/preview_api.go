package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func previewRouteEnabled(w http.ResponseWriter) bool {
	if !previewsLocallyEnabled() {
		http.NotFound(w, nil)
		return false
	}
	return true
}

func (a *apiServer) previewManagerOrError(w http.ResponseWriter) *previewManager {
	if !previewRouteEnabled(w) {
		return nil
	}
	if a.previews == nil {
		jsonError(w, http.StatusServiceUnavailable, "preview manager unavailable")
		return nil
	}
	return a.previews
}

func decodePreviewJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writePreviewError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var apiErr *previewHTTPError
	if errors.As(err, &apiErr) {
		status = apiErr.Status
	}
	jsonError(w, status, err.Error())
}

type previewCreateRequest struct {
	Port     int    `json:"port"`
	TaskID   string `json:"taskId,omitempty"`
	TTLHours *int   `json:"ttlHours,omitempty"`
}

func (a *apiServer) createPreview(w http.ResponseWriter, r *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	var body previewCreateRequest
	if err := decodePreviewJSON(w, r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		jsonError(w, http.StatusBadRequest, "an explicit loopback port is required")
		return
	}
	target, err := m.Create(r.Context(), uint16(body.Port), strings.TrimSpace(body.TaskID), body.TTLHours)
	if err != nil {
		writePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, target)
}

func (a *apiServer) listPreviews(w http.ResponseWriter, _ *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	writeJSON(w, http.StatusOK, m.List())
}

func (a *apiServer) stopPreview(w http.ResponseWriter, r *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	id := r.PathValue("previewId")
	if err := m.Stop(r.Context(), id); err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiServer) previewLink(w http.ResponseWriter, r *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	var body struct {
		TTLHours *int `json:"ttlHours,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := decodePreviewJSON(w, r, &body); err != nil {
			jsonError(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
	}
	link, err := m.MintLink(r.Context(), r.PathValue("previewId"), strings.HasSuffix(r.URL.Path, "/rotate-link"), body.TTLHours)
	if err != nil {
		writePreviewError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
}

func (a *apiServer) relaunchPreview(w http.ResponseWriter, r *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	var body struct {
		Port     int  `json:"port"`
		TTLHours *int `json:"ttlHours,omitempty"`
	}
	old, ok := m.Get(r.PathValue("previewId"))
	if !ok {
		jsonError(w, http.StatusNotFound, "unknown preview")
		return
	}
	if r.ContentLength != 0 {
		if err := decodePreviewJSON(w, r, &body); err != nil {
			jsonError(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
	}
	if old.Mode != previewModeAdopt {
		target, err := m.Relaunch(r.Context(), old.ID)
		if err != nil {
			writePreviewError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, target)
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		jsonError(w, http.StatusBadRequest, "adopt-mode relaunch requires an explicit port")
		return
	}
	if err := m.Stop(r.Context(), old.ID); err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	target, err := m.Create(r.Context(), uint16(body.Port), old.TaskID, body.TTLHours)
	if err != nil {
		writePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, target)
}

func previewLoopbackProxy(targetPort uint16, prefix string, targetBases ...string) *httputil.ReverseProxy {
	targetBase := ""
	if len(targetBases) > 0 {
		targetBase = targetBases[0]
	}
	targetURL := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(targetPort)))}
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			req := proxyReq.Out
			rest := strings.TrimPrefix(proxyReq.In.URL.Path, prefix)
			if rest == "" {
				rest = "/"
			}
			if !strings.HasPrefix(rest, "/") {
				rest = "/" + rest
			}
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = strings.TrimSuffix(targetBase, "/") + rest
			req.URL.RawPath = ""
			req.Host = targetURL.Host
			stripPreviewLoopbackRequest(req)
		},
		Transport: &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second},
	}
}

func previewMountedProxyHandler(targetPort uint16, prefix string, targetBases ...string) http.Handler {
	proxy := previewLoopbackProxy(targetPort, prefix, targetBases...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func writePreviewWaking(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="2"><title>Preview waking up…</title><p>Preview waking up…</p>`)
}

func writePreviewRelaunchFailed(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Preview unavailable</title><p>Could not relaunch — open Tasks.</p>`)
}

func stripPreviewLoopbackRequest(req *http.Request) {
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Forwarded")
	// Drop the retired marker as well as ordinary credentials.
	req.Header.Del("X-Soluterminal-Preview-Proxy")
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			req.Header.Del(name)
		}
	}
	var safeCookies []string
	for _, cookie := range req.Cookies() {
		name := strings.ToLower(cookie.Name)
		if strings.HasPrefix(name, "solu_") || strings.Contains(name, "preview") {
			continue
		}
		safeCookies = append(safeCookies, cookie.Name+"="+cookie.Value)
	}
	req.Header.Del("Cookie")
	if len(safeCookies) > 0 {
		req.Header.Set("Cookie", strings.Join(safeCookies, "; "))
	}
	stripPreviewBearerSubprotocols(req.Header)
	q := req.URL.Query()
	q.Del("access_token")
	q.Del("pv")
	req.URL.RawQuery = q.Encode()
}

func stripPreviewBearerSubprotocols(h http.Header) {
	var kept []string
	for _, raw := range h.Values("Sec-WebSocket-Protocol") {
		for _, proto := range strings.Split(raw, ",") {
			proto = strings.TrimSpace(proto)
			if proto != "" && !strings.HasPrefix(proto, "bearer.") {
				kept = append(kept, proto)
			}
		}
	}
	if len(kept) == 0 {
		h.Del("Sec-WebSocket-Protocol")
	} else {
		h.Set("Sec-WebSocket-Protocol", strings.Join(kept, ", "))
	}
}

func previewTargetAlive(ctx context.Context, port uint16) bool {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (a *apiServer) servePreview(w http.ResponseWriter, r *http.Request) {
	m := a.previewManagerOrError(w)
	if m == nil {
		return
	}
	id := r.PathValue("previewId")
	target, streamCtx, done, ok := m.beginRequest(r.Context(), id)
	if !ok {
		http.Error(w, "unknown preview", http.StatusForbidden)
		return
	}
	if target.State == previewStateIdle {
		if err := m.Wake(id); err != nil {
			writePreviewRelaunchFailed(w)
			return
		}
		writePreviewWaking(w)
		return
	}
	if target.State == previewStateStarting {
		writePreviewWaking(w)
		return
	}
	if target.State == previewStateError && target.Mode != previewModeAdopt {
		writePreviewRelaunchFailed(w)
		return
	}
	defer done()
	if !previewTargetAlive(streamCtx, target.TargetPort) {
		if target.Mode != previewModeAdopt {
			m.markOwnedError(id)
			writePreviewRelaunchFailed(w)
			return
		}
		if err := m.stop(context.Background(), id, true, false); err != nil {
			http.Error(w, "preview temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, "Preview expired — relaunch from Tasks\n")
		return
	}
	prefix := "/api/preview-serve/" + id
	targetBase := ""
	if target.PublicBase == "basePath" {
		targetBase = "/preview/" + id
	}
	proxy := previewLoopbackProxy(target.TargetPort, prefix, targetBase)
	// This route is on the app's own origin (loopback, router or the cloud
	// relay), so like /api/proxy its documents get an opaque origin. The
	// cloud's public preview origin (prefix /preview/<id>, which a browser
	// navigation cannot set) decides that itself (cloud/preview.go).
	if !strings.HasPrefix(r.Header.Get("X-Forwarded-Prefix"), "/preview/") {
		proxy.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Del("Content-Security-Policy-Report-Only")
			applyProxyDocumentSandbox(resp)
			return nil
		}
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		if target.Mode != previewModeAdopt {
			m.markOwnedError(id)
			writePreviewRelaunchFailed(rw)
			return
		}
		if err := m.stop(context.Background(), id, true, false); err != nil {
			http.Error(rw, "preview temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(rw, "Preview expired — relaunch from Tasks\n")
	}
	proxy.ServeHTTP(w, r.WithContext(streamCtx))
}
