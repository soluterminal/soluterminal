// Package brokererror projects provider failures without exposing request data
// and keeps transport/permission failures separate from account-scoped failures.
package brokererror

import (
	"encoding/json"
	"io"
	"net/http"
)

// WriteProviderError is the cloud's provider-edge projection. A bare status
// from a CDN is not evidence that the provider rejected an account credential.
func WriteProviderError(w http.ResponseWriter, upstream *http.Response) {
	kind := "provider_transport_error"
	if upstream.Body != nil {
		data, err := io.ReadAll(io.LimitReader(upstream.Body, (64<<10)+1))
		var shape struct {
			Type  string `json:"type"`
			Error *struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err == nil && len(data) <= 64<<10 && json.Unmarshal(data, &shape) == nil && shape.Error != nil {
			kind = "provider_error"
			// Structured provider errors retain useful status diagnostics. Permission
			// and billing are deliberately not classified as account authentication.
			switch upstream.StatusCode {
			case http.StatusPaymentRequired:
				kind = "billing_error"
			case http.StatusForbidden:
				kind = "permission_error"
			}
			for _, candidate := range []string{shape.Error.Code, shape.Error.Type, shape.Type} {
				matched := true
				switch candidate {
				case "authentication_error", "invalid_api_key", "invalid_token", "token_expired", "token_revoked", "token_invalidated":
					kind = "authentication_error"
				case "rate_limit_error", "rate_limit_exceeded", "insufficient_quota", "usage_limit_reached":
					kind = "rate_limit_error"
				case "server_overloaded", "server_is_overloaded", "overloaded_error":
					kind = "server_overloaded"
				case "billing_error", "billing_not_active":
					kind = "billing_error"
				case "permission_error", "permission_denied":
					kind = "permission_error"
				default:
					matched = false
				}
				if matched {
					break
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(upstream.StatusCode)
	WriteBody(w, kind)
}

// Messages and codes are fixed local copy, never provider-supplied prose.
// Keep quota, capacity and a rejected login distinct across both broker hops.
func WriteBody(w http.ResponseWriter, kind string) {
	message := kind
	switch kind {
	case "authentication_refreshed":
		message = "The shared account login was refreshed. Continue this conversation to use the renewed login."
	case "authentication_error":
		message = "The shared account login was rejected. Check Credentials; the team owner may need to renew this account."
	case "rate_limit_error":
		message = "The shared account has reached a usage limit. Check Credentials for remaining quota and reset time."
	case "server_overloaded":
		message = "The selected model is temporarily at capacity. Try again shortly."
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "code": kind, "message": message}})
}

// RejectedRefreshGrant distinguishes an explicit OAuth grant rejection from
// network, edge, throttle and service failures. Only the former needs login.
func RejectedRefreshGrant(status int, data []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &payload) != nil {
		return false
	}
	var code string
	if json.Unmarshal(payload.Error, &code) != nil {
		var e struct {
			Code string `json:"code"`
			Type string `json:"type"`
		}
		if json.Unmarshal(payload.Error, &e) != nil {
			return false
		}
		code = e.Code
		if code == "" {
			code = e.Type
		}
	}
	switch code {
	case "invalid_grant", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated", "token_expired", "token_revoked":
		return true
	}
	return false
}

// RejectedRefreshAuth reports a structured 401 or 403 from the token endpoint
// that RejectedRefreshGrant doesn't claim. The token endpoint or something in
// front of it answered, but not about this grant, so callers treat it as
// temporary and back off. A bare status or an HTML page isn't an answer at all.
func RejectedRefreshAuth(status int, data []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &payload) != nil {
		return false
	}
	return len(payload.Error) > 0 && string(payload.Error) != "null"
}

// AccountFailureStatus maps only supported account-scoped classes to the
// existing reactive recovery categories. Permission, billing, and transport
// failures require diagnostics: switching accounts cannot establish their scope.
func AccountFailureStatus(status int, kind string) int {
	switch kind {
	case "authentication_error":
		if status == 400 || status == 401 || status == 403 || status == 422 {
			return http.StatusUnauthorized
		}
	case "rate_limit_error":
		if status == 400 || status == 422 || status == 429 {
			return http.StatusTooManyRequests
		}
	}
	return 0
}
