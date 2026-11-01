// Package brokerbody keeps the desktop and cloud model-request budgets and
// rejection messages consistent. It never removes images or rewrites prompts.
package brokerbody

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	DefaultLimit int64 = 32 << 20
	// Codex retains inline image previews in subsequent Responses requests.
	// A text-only turn can therefore carry far more than 32 MiB of history.
	CodexResponsesLimit int64 = 128 << 20
)

var (
	ErrInvalidJSON = errors.New("invalid broker JSON")
	ErrEncoding    = errors.New("unsupported broker encoding")
)

func Limit(provider, operation string) int64 {
	if provider == "codex" && operation == "responses" {
		return CodexResponsesLimit
	}
	return DefaultLimit
}

// WireLimit allows gzip framing and incompressible-data overhead. The decoded
// body is independently bounded by Limit; gzip cannot bypass that budget.
func WireLimit(limit int64) int64 { return limit + (1 << 20) }

func ReadJSON(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return nil, ErrEncoding
	}
	declaredLimit := limit
	if encoding == "gzip" {
		declaredLimit = WireLimit(limit)
	}
	if r.ContentLength > declaredLimit {
		return nil, &http.MaxBytesError{Limit: declaredLimit}
	}
	r.Body = http.MaxBytesReader(w, r.Body, WireLimit(limit))
	var reader io.Reader = r.Body
	switch encoding {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	default:
		return nil, ErrEncoding
	}
	// A typed MaxBytesError distinguishes body limits from malformed JSON,
	// including when a gzip body exceeds the decoded rather than wire budget.
	body, err := io.ReadAll(http.MaxBytesReader(w, io.NopCloser(reader), limit))
	if err != nil {
		return body, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return body, ErrInvalidJSON
	}
	return body, nil
}

func ErrorType(err error) string {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return "request_too_large"
	case errors.Is(err, ErrInvalidJSON):
		return "invalid_request_error"
	case errors.Is(err, ErrEncoding):
		return "unsupported_content_encoding"
	default:
		return "request_read_error"
	}
}

// WriteError emits only fixed messages and byte budgets, never request content
// or underlying I/O errors that might contain prompts or credentials.
func WriteError(w http.ResponseWriter, err error, limit int64) {
	status, message := http.StatusBadRequest, "SoluTerminal could not read the model request body."
	kind := ErrorType(err)
	switch kind {
	case "request_too_large":
		status = http.StatusRequestEntityTooLarge
		message = fmt.Sprintf("SoluTerminal's credential service request limit is %d MiB. Image previews retained in conversation history count toward this limit, even on a short text turn. Compact the conversation with /compact, or continue in a fresh chat using the saved files.", limit>>20)
	case "invalid_request_error":
		message = "SoluTerminal requires a valid JSON object for the model request."
	case "unsupported_content_encoding":
		status = http.StatusUnsupportedMediaType
		message = "SoluTerminal's credential service accepts plain JSON or gzip request bodies."
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	response := map[string]any{"type": kind, "code": kind, "message": message}
	if kind == "request_too_large" {
		response["max_bytes"] = limit
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": response})
}
