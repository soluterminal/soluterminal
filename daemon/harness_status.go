package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"soluterminal-shared/providers"
	"strings"
	"sync"
	"time"
)

// harnessEndpoint is the provider API root for the selected account's billing
// mode, used by Check/Test, the balance query and the model gateway. Endpoints
// come from the provider facts table; an unknown provider or billing mode is
// rejected. For DeepSeek this is the root API (Chat Completions, /models,
// /user/balance); only the DeepSeek Harness launcher uses the Messages root
// under /anthropic (launch.mjs endpoint()).
func harnessEndpoint(provider, billing string) (string, error) {
	p, ok := providers.Get(provider)
	if !ok || p.Harness == nil {
		return "", errors.New("unsupported provider or billing type")
	}
	if base := p.Harness.Endpoints[billing]; base != "" {
		return base, nil
	}
	return "", errors.New("unsupported provider or billing type")
}

// harnessTestResult is metadata about one explicit Test Connection. It never
// carries the key or any provider response body.
type harnessTestResult struct {
	Status     string `json:"status"`
	HTTPStatus int    `json:"httpStatus"`
	At         int64  `json:"at"`
	KeyHash    string `json:"keyHash,omitempty"`
	Model      string `json:"model,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// harnessTestDTO is the browser-facing view: the key hash stays local.
type harnessTestDTO struct {
	Status     string `json:"status"`
	HTTPStatus int    `json:"httpStatus"`
	At         int64  `json:"at"`
	Model      string `json:"model,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func (r harnessTestResult) DTO() harnessTestDTO {
	return harnessTestDTO{Status: r.Status, HTTPStatus: r.HTTPStatus, At: r.At, Model: r.Model, Detail: r.Detail}
}

type harnessTestState struct {
	Version int                          `json:"version"`
	Results map[string]harnessTestResult `json:"results"`
}

type harnessTestStore struct {
	mu   sync.Mutex
	path string
}

func newHarnessTestStore(dataDir, provider string) *harnessTestStore {
	return &harnessTestStore{path: filepath.Join(dataDir, "harness-accounts", provider+"-tests.json")}
}

func harnessKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *harnessTestStore) readLocked() map[string]harnessTestResult {
	if info, err := os.Stat(s.path); err == nil && info.Size() > 1<<20 {
		return map[string]harnessTestResult{}
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]harnessTestResult{}
	}
	st := harnessTestState{}
	if json.Unmarshal(b, &st) != nil || st.Version != 1 || st.Results == nil {
		return map[string]harnessTestResult{}
	}
	return st.Results
}

// get returns a stored result only when it still describes the current key and
// model, so a rotated key or an edited model reads as not-tested.
func (s *harnessTestStore) get(key string, acc harnessAccount) (harnessTestResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.readLocked()[key]
	if !ok || r.KeyHash != harnessKeyHash(acc.APIKey) || r.Model != acc.Model {
		return harnessTestResult{}, false
	}
	return r, true
}

func (s *harnessTestStore) put(key string, r harnessTestResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := harnessTestState{Version: 1, Results: s.readLocked()}
	st.Results[key] = r
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0600)
}

// selected returns the currently selected account: the only account a launch or
// a Test Connection may use. No fallback to an unselected personal or ambient
// credential.
func (s *harnessCredStore) selected() (source, name string, acc harnessAccount, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return "", "", harnessAccount{}, err
	}
	source, name, ok := strings.Cut(st.Selected, "/")
	if !ok || name == "" {
		return "", "", harnessAccount{}, errors.New("select an API account first")
	}
	accounts := st.Personal
	if source == "team" {
		accounts = st.Team
	}
	acc, ok = accounts[name]
	if !ok {
		return "", "", harnessAccount{}, errors.New("the selected harness account is missing")
	}
	if source == "team" {
		return source, name, acc, validateHarnessTeamAccount(s.provider, acc)
	}
	return source, name, acc, validateHarnessAccount(s.provider, acc)
}

// probeHarnessAccount sends one minimal completion against the account's own
// endpoint. That is a real (tiny) paid request, so it must only run from an
// explicit user Test action — never a schedule. It classifies the outcome and
// deliberately never reads or returns the response body.
func probeHarnessAccount(ctx context.Context, base string, acc harnessAccount, client *http.Client) harnessTestResult {
	return probeHarnessAccountForProvider(ctx, base, "deepseek", acc, client)
}

func probeHarnessAccountForProvider(ctx context.Context, base, provider string, acc harnessAccount, client *http.Client) harnessTestResult {
	res := harnessTestResult{At: time.Now().Unix(), KeyHash: harnessKeyHash(acc.APIKey), Model: acc.Model}
	if provider == "deepseek" || provider == "pi" {
		model, err := providers.CanonicalDeepSeekDispatchModel(acc.Model)
		if err != nil {
			res.Status = "error"
			res.Detail = err.Error()
			return res
		}
		acc.Model, res.Model = model, model
	} else if providers.HasRetiredDeepSeekProReference(acc.Model) {
		res.Status = "error"
		res.Detail = "retired DeepSeek model cannot be probed through this provider"
		return res
	}
	body, err := json.Marshal(map[string]any{
		"model":      acc.Model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		res.Status = "error"
		res.Detail = "could not build the request"
		return res
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		res.Status = "error"
		res.Detail = "could not build the request"
		return res
	}
	req.Header.Set("Authorization", "Bearer "+acc.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			res.Status = "timeout"
			res.Detail = "the provider did not answer in time"
		} else {
			res.Status = "unreachable"
			res.Detail = "could not reach the provider"
		}
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Status = "authenticated"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Status = "auth-failed"
		res.Detail = "the provider rejected the key"
	case resp.StatusCode == 402:
		res.Status = "billing"
		res.Detail = "the account has no usable balance"
	case resp.StatusCode == 429:
		res.Status = "rate-limited"
		res.Detail = "the provider rate-limited the test"
	case resp.StatusCode >= 500:
		res.Status = "unavailable"
		res.Detail = "the provider reported a server error"
	default:
		res.Status = "error"
		res.Detail = "the provider rejected the test"
	}
	return res
}

// probeHarnessAuth verifies the key is accepted without spending: it reads the
// provider's model list. A 404/405 means the provider exposes no models route,
// reported as "unsupported" so the UI can point at the paid Test instead. Like
// the completion probe, it never reads or returns the response body.
func probeHarnessAuth(ctx context.Context, base string, acc harnessAccount, client *http.Client) harnessTestResult {
	res := harnessTestResult{At: time.Now().Unix(), KeyHash: harnessKeyHash(acc.APIKey), Model: acc.Model}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		res.Status = "error"
		res.Detail = "could not build the request"
		return res
	}
	req.Header.Set("Authorization", "Bearer "+acc.APIKey)
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			res.Status = "timeout"
			res.Detail = "the provider did not answer in time"
		} else {
			res.Status = "unreachable"
			res.Detail = "could not reach the provider"
		}
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Status = "authenticated"
		res.Detail = "the key was accepted"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Status = "auth-failed"
		res.Detail = "the provider rejected the key"
	case resp.StatusCode == 402:
		res.Status = "billing"
		res.Detail = "the account has no usable balance"
	case resp.StatusCode == 429:
		res.Status = "rate-limited"
		res.Detail = "the provider rate-limited the check"
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		res.Status = "unsupported"
		res.Detail = "this provider has no models route; use Test"
	case resp.StatusCode >= 500:
		res.Status = "unavailable"
		res.Detail = "the provider reported a server error"
	default:
		res.Status = "error"
		res.Detail = "the provider rejected the check"
	}
	return res
}
