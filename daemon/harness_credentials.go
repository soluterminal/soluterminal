package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"soluterminal-shared/providers"
	"sort"
	"strings"
	"sync"
	"time"
)

// These types are disk/secret transport only. Browser responses use
// harnessAccountDTO and never marshal an account or state directly.
type harnessAccount struct {
	Name       string `json:"name"`
	APIKey     string `json:"apiKey"`
	Billing    string `json:"billing"`
	Model      string `json:"model"`
	BrokerOnly bool   `json:"brokerOnly,omitempty"`
}

type harnessAccountState struct {
	Version  int                       `json:"version"`
	Personal map[string]harnessAccount `json:"personal"`
	Team     map[string]harnessAccount `json:"team"`
	Selected string                    `json:"selected"`
	Scope    string                    `json:"scope"`
	TeamID   string                    `json:"teamId"`
	SyncedAt int64                     `json:"syncedAt"`
}

type harnessAccountDTO struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Selected bool   `json:"selected"`
	Billing  string `json:"billing"`
	Model    string `json:"model"`
}

type harnessCredStore struct {
	mu             sync.Mutex
	syncMu         sync.Mutex
	path, provider string
	dataDir        string
}

var errHarnessAccount = errors.New("invalid harness account: use a filename-safe name, a single-line API key, and a supported billing type")

func isHarnessProvider(provider string) bool { return providers.IsHarness(provider) }

// harnessBillingAllowed reports whether billing is one of the provider's
// declared modes. The facts table is the single source of truth; the daemon no
// longer special-cases DeepSeek at the call site.
func harnessBillingAllowed(h *providers.HarnessFacts, billing string) bool {
	for _, allowed := range h.Billings {
		if allowed == billing {
			return true
		}
	}
	return false
}

func validateHarnessAccountBase(provider string, acc harnessAccount) error {
	p, ok := providers.Get(provider)
	if !ok || p.Harness == nil || !validCredName(acc.Name) || acc.Name == "__team__" ||
		strings.TrimSpace(acc.APIKey) == "" || len(acc.APIKey) > 8192 || strings.ContainsAny(acc.APIKey, "\r\n\x00") ||
		!harnessBillingAllowed(p.Harness, acc.Billing) ||
		strings.TrimSpace(acc.Model) == "" || len(acc.Model) > 160 || strings.ContainsAny(acc.Model, "\r\n\x00") {
		return errHarnessAccount
	}
	return nil
}

// validateHarnessAccount is the new-write boundary. DeepSeek-backed harnesses
// accept current canonical selectors only; stored/team legacy data uses the
// separate migration path below.
func validateHarnessAccount(provider string, acc harnessAccount) error {
	if err := validateHarnessAccountBase(provider, acc); err != nil {
		return err
	}
	if provider == "deepseek" || provider == "pi" {
		if err := providers.ValidateDeepSeekSelector(acc.Model); err != nil {
			return fmt.Errorf("%w: %v", errHarnessAccount, err)
		}
	} else if providers.HasRetiredDeepSeekReference(acc.Model) {
		return fmt.Errorf("%w: retired DeepSeek model is not valid for provider %q", errHarnessAccount, provider)
	}
	return nil
}

// Cloud-supplied credentials can be keyless on this machine. Validate their
// metadata with the same model rules as personal accounts without requiring a
// provider API key to exist locally.
func validateHarnessTeamAccount(provider string, acc harnessAccount) error {
	if !acc.BrokerOnly {
		return validateHarnessAccount(provider, acc)
	}
	p, ok := providers.Get(provider)
	if !ok || p.Harness == nil || !validCredName(acc.Name) || acc.Name == "__team__" || acc.APIKey != "" ||
		!harnessBillingAllowed(p.Harness, acc.Billing) || strings.TrimSpace(acc.Model) == "" ||
		len(acc.Model) > 160 || strings.ContainsAny(acc.Model, "\r\n\x00") {
		return errHarnessAccount
	}
	if provider == "deepseek" || provider == "pi" {
		if err := providers.ValidateDeepSeekSelector(acc.Model); err != nil {
			return fmt.Errorf("%w: %v", errHarnessAccount, err)
		}
	} else if providers.HasRetiredDeepSeekReference(acc.Model) {
		return fmt.Errorf("%w: retired DeepSeek model is not valid for provider %q", errHarnessAccount, provider)
	}
	return nil
}

// normalizeStoredHarnessModel is migration-only. New PUT/import writes use
// validateHarnessAccount and may never silently convert a retired selection.
func normalizeStoredHarnessModel(provider, model string) (string, error) {
	if provider != "deepseek" && provider != "pi" {
		if providers.HasRetiredDeepSeekReference(model) {
			return "", fmt.Errorf("retired DeepSeek model %q is invalid for provider %q", model, provider)
		}
		return model, nil
	}
	return providers.CanonicalDeepSeekDispatchModel(model)
}

func newHarnessCredStore(dataDir, provider string) *harnessCredStore {
	path := filepath.Join(dataDir, "harness-accounts", provider+".json")
	// A store file is replaced via an atomic scratch file that carries the same
	// secret as the store. A crash or SIGKILL between write and rename leaves
	// that scratch file behind, so sweep stale ones whenever a store is first
	// opened. The age floor keeps an in-flight write in another process safe.
	sweepStaleHarnessTempFiles(filepath.Dir(path), time.Now())
	return &harnessCredStore{path: path, provider: provider, dataDir: dataDir}
}

// harnessTempFileGrace is how long an atomic-write scratch file must sit
// untouched before it is treated as a crash leftover rather than an in-flight
// write. Real writes finish in milliseconds; an hour is far beyond any.
const harnessTempFileGrace = time.Hour

// sweepStaleHarnessTempFiles removes atomic-write scratch files (".<name>.tmp-*")
// left under dir by a previous crash. They can contain API keys, so they must
// not persist. It is intentionally best-effort: a file that cannot be read or
// removed is skipped rather than failing the credential read that triggered it.
func sweepStaleHarnessTempFiles(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".") || !strings.Contains(name, ".tmp-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < harnessTempFileGrace {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func (s *harnessCredStore) readLocked() (harnessAccountState, error) {
	st := harnessAccountState{Version: 1, Personal: map[string]harnessAccount{}, Team: map[string]harnessAccount{}}
	if info, err := os.Stat(s.path); err == nil && info.Size() > 3<<20 {
		return st, errors.New("harness credential store is too large; it was not overwritten")
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, errors.New("cannot read harness credential store")
	}
	if json.Unmarshal(b, &st) != nil || st.Version != 1 {
		return st, errors.New("harness credential store is malformed; it was not overwritten")
	}
	if st.Personal == nil {
		st.Personal = map[string]harnessAccount{}
	}
	if st.Team == nil {
		st.Team = map[string]harnessAccount{}
	}
	// Secrets on disk are sealed; open them before validation so the checks run
	// against the real key. A decrypt failure is a hard error: the store is not
	// overwritten with an empty or partial state.
	if err := s.openAccounts(st.Personal); err != nil {
		return st, err
	}
	if err := s.openAccounts(st.Team); err != nil {
		return st, err
	}
	migrated := false
	for source, accounts := range map[string]map[string]harnessAccount{"personal": st.Personal, "team": st.Team} {
		if len(accounts) > 128 {
			return st, errors.New("too many harness accounts")
		}
		for name, account := range accounts {
			normalized, normalizeErr := normalizeStoredHarnessModel(s.provider, account.Model)
			if normalizeErr != nil {
				return st, errors.New("harness credential store contains an unsupported model; it was not overwritten")
			}
			if normalized != account.Model {
				account.Model = normalized
				accounts[name] = account
				migrated = true
			}
			valid := validateHarnessAccountBase(s.provider, account)
			if source == "team" {
				valid = validateHarnessTeamAccount(s.provider, account)
			}
			if name != account.Name || valid != nil {
				return st, errors.New("harness credential store contains an invalid account; it was not overwritten")
			}
		}
	}
	if migrated {
		if err := s.writeLocked(st); err != nil {
			return st, errors.New("cannot persist harness credential model migration")
		}
	}
	return st, nil
}

func (s *harnessCredStore) writeLocked(st harnessAccountState) error {
	// The ZCode live config needs the real key, so reconcile before sealing.
	if err := s.reconcileZcodeLiveKey(st); err != nil {
		return err
	}
	disk := st
	var err error
	if disk.Personal, err = s.sealAccounts(st.Personal); err != nil {
		return err
	}
	if disk.Team, err = s.sealAccounts(st.Team); err != nil {
		return err
	}
	b, err := json.Marshal(disk)
	if err != nil {
		return errors.New("cannot encode harness credential store")
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0600)
}

// sealAccounts returns a copy of accounts with every API key sealed for disk.
// The key is resolved once per call; an empty set needs no key at all.
func (s *harnessCredStore) sealAccounts(accounts map[string]harnessAccount) (map[string]harnessAccount, error) {
	if len(accounts) == 0 {
		return map[string]harnessAccount{}, nil
	}
	needsKey := false
	for _, acc := range accounts {
		if !(acc.BrokerOnly && acc.APIKey == "") {
			needsKey = true
			break
		}
	}
	if !needsKey {
		out := make(map[string]harnessAccount, len(accounts))
		for name, acc := range accounts {
			out[name] = acc
		}
		return out, nil
	}
	key, err := harnessSecretKey(s.dataDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]harnessAccount, len(accounts))
	for name, acc := range accounts {
		if acc.BrokerOnly && acc.APIKey == "" {
			out[name] = acc
			continue
		}
		sealed, err := sealHarnessSecret(key, acc.APIKey)
		if err != nil {
			return nil, err
		}
		acc.APIKey = sealed
		out[name] = acc
	}
	return out, nil
}

// openAccounts decrypts sealed API keys in place. A store with no sealed value
// (a fresh or entirely legacy file) is left untouched and never provisions a
// key, so a read alone cannot create key material.
func (s *harnessCredStore) openAccounts(accounts map[string]harnessAccount) error {
	sealed := false
	for _, acc := range accounts {
		if strings.HasPrefix(acc.APIKey, harnessSecretPrefix) {
			sealed = true
			break
		}
	}
	if !sealed {
		return nil
	}
	key, err := harnessSecretKey(s.dataDir)
	if err != nil {
		return err
	}
	for name, acc := range accounts {
		plain, err := openHarnessSecret(key, acc.APIKey)
		if err != nil {
			return err
		}
		acc.APIKey = plain
		accounts[name] = acc
	}
	return nil
}

// ZCode's interactive checker requires a live provider.zai API key in its
// normal config. Remove only a slot written by this store and still bearing
// our exact key. User-replaced keys and unrelated providers remain untouched.
func (s *harnessCredStore) reconcileZcodeLiveKey(st harnessAccountState) error {
	if s.provider != "glm" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return reconcileZcodeConfig(filepath.Join(home, ".zcode", "cli", "config.json"), s.path, st)
}

func reconcileZcodeConfig(file, storePath string, st harnessAccountState) error {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("could not reconcile the managed ZCode key")
	}
	var config map[string]json.RawMessage
	if json.Unmarshal(b, &config) != nil {
		return errors.New("ZCode config is malformed; managed key cleanup needs attention")
	}
	var marker struct {
		StorePath, Source, Name, Billing, Model, Scope, KeyHash string
		BrokerOnly                                              bool
	}
	if json.Unmarshal(config["soluterminalManagedGLM"], &marker) != nil || marker.StorePath != storePath {
		return nil
	}
	accounts := st.Personal
	if marker.Source == "team" {
		accounts = st.Team
	}
	acc, exists := accounts[marker.Name]
	hash := sha256.Sum256([]byte(acc.APIKey))
	keyMatches := hex.EncodeToString(hash[:]) == marker.KeyHash || (marker.BrokerOnly && marker.Source == "team" && acc.BrokerOnly && acc.APIKey == "")
	if exists && st.Selected == marker.Source+"/"+marker.Name && acc.Billing == marker.Billing && acc.Model == marker.Model && keyMatches && (marker.Source != "team" || marker.Scope == st.Scope) {
		return nil
	}
	var providers map[string]json.RawMessage
	var zai map[string]json.RawMessage
	var options map[string]json.RawMessage
	var key string
	if json.Unmarshal(config["provider"], &providers) != nil || json.Unmarshal(providers["zai"], &zai) != nil || json.Unmarshal(zai["options"], &options) != nil {
		return errors.New("could not reconcile the managed ZCode provider")
	}
	_ = json.Unmarshal(options["apiKey"], &key)
	hash = sha256.Sum256([]byte(key))
	if hex.EncodeToString(hash[:]) == marker.KeyHash {
		delete(options, "apiKey")
		zai["options"], _ = json.Marshal(options)
		providers["zai"], _ = json.Marshal(zai)
		config["provider"], _ = json.Marshal(providers)
	}
	delete(config, "soluterminalManagedGLM")
	b, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(file, b, 0600)
}

func (s *harnessCredStore) mutate(fn func(*harnessAccountState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return err
	}
	if err = fn(&st); err != nil {
		return err
	}
	return s.writeLocked(st)
}

// clearLegacyTeamSecrets removes distributed team API keys even while the
// cloud is offline. The selected team row is cleared rather than falling back
// to a personal key; a later metadata sync restores ready assignments.
func (s *harnessCredStore) clearLegacyTeamSecrets() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return err
	}
	changed := false
	for label, account := range st.Team {
		if account.BrokerOnly && account.APIKey == "" {
			continue
		}
		delete(st.Team, label)
		if st.Selected == "team/"+label {
			st.Selected = ""
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return s.writeLocked(st)
}

func (s *harnessCredStore) list() ([]harnessAccountDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	out := []harnessAccountDTO{}
	for source, accounts := range map[string]map[string]harnessAccount{"personal": st.Personal, "team": st.Team} {
		for name, acc := range accounts {
			out = append(out, harnessAccountDTO{Name: name, Source: source, Selected: st.Selected == source+"/"+name, Billing: acc.Billing, Model: acc.Model})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *harnessCredStore) save(acc harnessAccount) error {
	acc.APIKey = strings.TrimSpace(acc.APIKey)
	if err := validateHarnessAccount(s.provider, acc); err != nil {
		return err
	}
	return s.mutate(func(st *harnessAccountState) error {
		if _, exists := st.Personal[acc.Name]; !exists && len(st.Personal) >= 128 {
			return errors.New("too many personal harness accounts")
		}
		st.Personal[acc.Name] = acc
		if st.Selected == "" {
			st.Selected = "personal/" + acc.Name
		}
		return nil
	})
}

func (s *harnessCredStore) selectAccount(source, name string) error {
	return s.mutate(func(st *harnessAccountState) error {
		accounts := st.Personal
		if source == "team" {
			accounts = st.Team
		} else if source != "personal" {
			return errHarnessAccount
		}
		acc, ok := accounts[name]
		if !ok {
			return errors.New("harness account not found")
		}
		if source == "team" {
			if err := validateHarnessTeamAccount(s.provider, acc); err != nil {
				return err
			}
		} else if err := validateHarnessAccount(s.provider, acc); err != nil {
			return err
		}
		st.Selected = source + "/" + name
		return nil
	})
}

func (s *harnessCredStore) remove(name string) error {
	return s.mutate(func(st *harnessAccountState) error {
		delete(st.Personal, name)
		if st.Selected == "personal/"+name {
			st.Selected = ""
		}
		return nil
	})
}

func (s *harnessCredStore) personal(name string) (harnessAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return harnessAccount{}, err
	}
	acc, ok := st.Personal[name]
	if !ok {
		return harnessAccount{}, errors.New("personal source account not found")
	}
	return acc, validateHarnessAccount(s.provider, acc)
}

func (s *harnessCredStore) selectedTeamID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(st.Selected, "team/") || st.TeamID == "" {
		return "", errors.New("no selected team assignment")
	}
	return st.TeamID, nil
}

// account returns a named personal key without changing the selected account.
// Team assignments have no member-side provider key and must use the broker.
func (s *harnessCredStore) account(source, name string) (harnessAccount, error) {
	if source != "personal" || !validCredName(name) {
		return harnessAccount{}, errors.New("account unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return harnessAccount{}, err
	}
	acc, ok := st.Personal[name]
	if !ok {
		return harnessAccount{}, errors.New("account unavailable")
	}
	return acc, nil
}

// A pairing change invalidates every previously assigned key. An empty
// selection deliberately has no fallback to a personal/ambient credential.
func (s *harnessCredStore) bindScope(scope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readLocked()
	if err != nil {
		return err
	}
	if st.Scope == scope {
		return nil
	}
	st.Scope, st.TeamID, st.Team = scope, "", map[string]harnessAccount{}
	if strings.HasPrefix(st.Selected, "team/") {
		st.Selected = ""
	}
	return s.writeLocked(st)
}

// harnessSelectionResolves reports whether st.Selected points at a real account.
// A store can carry a stale pointer (a removed personal account, or a team
// account dropped from the set); the launcher treats that as no selection, so
// the sync treats it the same way.
func harnessSelectionResolves(st *harnessAccountState) bool {
	parts := strings.SplitN(st.Selected, "/", 3)
	if len(parts) != 2 || parts[1] == "" {
		return false
	}
	switch parts[0] {
	case "personal":
		_, ok := st.Personal[parts[1]]
		return ok
	case "team":
		_, ok := st.Team[parts[1]]
		return ok
	default:
		return false
	}
}

// firstHarnessTeamName picks a deterministic cloud-supplied credential when
// no usable selection exists. Sorted names keep synchronization stable.
func firstHarnessTeamName(accounts map[string]harnessAccount) (string, bool) {
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

func (s *harnessCredStore) replaceTeam(scope, teamID string, accounts map[string]harnessAccount) error {
	if len(accounts) > 128 {
		return errors.New("too many assigned harness accounts")
	}
	for name, acc := range accounts {
		normalized, err := normalizeStoredHarnessModel(s.provider, acc.Model)
		if err != nil {
			return err
		}
		acc.Model = normalized
		accounts[name] = acc
		if acc.Name != name {
			return errHarnessAccount
		}
		if err := validateHarnessTeamAccount(s.provider, acc); err != nil {
			return err
		}
	}
	return s.mutate(func(st *harnessAccountState) error {
		if st.Scope != scope {
			return errors.New("pairing changed during team sync; retry")
		}
		st.Team, st.TeamID, st.SyncedAt = accounts, teamID, time.Now().Unix()
		if strings.HasPrefix(st.Selected, "team/") {
			if _, ok := accounts[strings.TrimPrefix(st.Selected, "team/")]; !ok {
				st.Selected = ""
			}
		}
		// Match the other providers: use a cloud-supplied credential when
		// no usable selection exists, so initial setup needs no manual pick.
		// A valid local selection is never overridden.
		if !harnessSelectionResolves(st) {
			if name, ok := firstHarnessTeamName(accounts); ok {
				st.Selected = "team/" + name
			}
		}
		return nil
	})
}
