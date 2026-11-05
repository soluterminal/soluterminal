package brokererror

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderErrorProjection(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		status, recovery int
	}{
		{"provider edge HTML403", "<html>synthetic-secret</html>", "provider_transport_error", 403, 0},
		{"generic permission403", `{"error":{"message":"synthetic-secret"}}`, "permission_error", 403, 0},
		{"model permission403", `{"error":{"type":"permission_error","message":"synthetic-secret"}}`, "permission_error", 403, 0},
		{"untyped auth401", `{"error":{"message":"synthetic-secret"}}`, "provider_error", 401, 0},
		{"untyped quota429", `{"error":{"message":"synthetic-secret"}}`, "provider_error", 429, 0},
		{"expired access", `{"error":{"code":"token_expired"}}`, "authentication_error", 401, 401},
		{"weekly usage", `{"error":{"type":"usage_limit_reached"}}`, "rate_limit_error", 429, 429},
		{"capacity", `{"error":{"code":"server_overloaded"}}`, "server_overloaded", 503, 0},
		{"auth401", `{"error":{"type":"authentication_error","message":"synthetic-secret"}}`, "authentication_error", 401, 401},
		{"quota429", `{"error":{"type":"rate_limit_error","message":"synthetic-secret"}}`, "rate_limit_error", 429, 429},
		{"quota400", `{"error":{"code":"rate_limit_exceeded","message":"synthetic-secret"}}`, "rate_limit_error", 400, 429},
		{"quota422", `{"error":{"type":"rate_limit_error","message":"synthetic-secret"}}`, "rate_limit_error", 422, 429},
		{"billing402", `{"error":{"type":"billing_error","message":"synthetic-secret"}}`, "billing_error", 402, 0},
		{"unstructured JSON403", `{"message":"synthetic-secret"}`, "provider_transport_error", 403, 0},
		{"oversized JSON403", `{"error":{"type":"authentication_error","message":"` + strings.Repeat("x", 65536) + `"}}`, "provider_transport_error", 403, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteProviderError(w, &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))})
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.kind) || strings.Contains(w.Body.String(), "synthetic-secret") {
				t.Fatalf("projection %d %s", w.Code, w.Body.String())
			}
			if got := AccountFailureStatus(tc.status, tc.kind); got != tc.recovery {
				t.Fatalf("recovery=%d want %d", got, tc.recovery)
			}
		})
	}
}

func TestRejectedRefreshGrant(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":"invalid_grant"}`, true},
		{401, `{"error":{"code":"refresh_token_reused"}}`, true},
		{400, `{"error":{"type":"refresh_token_expired"}}`, true},
		{429, `{"error":"invalid_grant"}`, false},
		{500, `{"error":"invalid_grant"}`, false},
		{401, `<html>invalid_grant</html>`, false},
		{401, `{"error":{"message":"invalid_grant synthetic-secret"}}`, false},
	} {
		if got := RejectedRefreshGrant(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("status %d body %s got %t", tc.status, tc.body, got)
		}
	}
}

func TestRejectedRefreshAuth(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{401, `{"error":"invalid_client"}`, true},
		{403, `{"error":{"type":"permission_error"}}`, true},
		{403, `<html>edge block</html>`, false},
		{401, `<html>edge block</html>`, false},
		{403, ``, false},
		{403, `{"message":"no error field"}`, false},
		{403, `{"error":null}`, false},
		{400, `{"error":"invalid_request"}`, false},
		{429, `{"error":"rate_limited"}`, false},
		{500, `{"error":"server_error"}`, false},
	} {
		if got := RejectedRefreshAuth(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("status %d body %s got %t", tc.status, tc.body, got)
		}
	}
}
