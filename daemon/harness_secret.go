package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// At-rest protection for harness API keys. The store file previously held the
// raw key, so any read as the OS user (a backup, a grep, a pasted debug file)
// exposed every personal and delivered team key. API keys are now sealed with
// AES-256-GCM before they are written.
//
// The encryption key comes from the OS keychain when one is available
// (`security` on macOS, `secret-tool` on Linux/libsecret). Where there is no
// keychain -- WSL, a container, a headless cloud box -- it falls back to a
// 0600 key file beside the store. The fallback does not change the local threat
// model (a same-user attacker can read the key too); it removes plaintext keys
// from the data directory and from anything that copies a single store file.
// The keychain backend is the meaningful upgrade on a desktop session.

// harnessSecretPrefix marks a sealed value. Anything without it is a legacy
// plaintext value and is accepted on read, then sealed on the next write.
const harnessSecretPrefix = "enc:v1:"

// harnessSecretKeyBytes is the AES-256 key length.
const harnessSecretKeyBytes = 32

// harnessSecretBackend is the OS secret store seam. The zero value behaves as
// "no keychain"; tests substitute a fake.
type harnessSecretBackend interface {
	Get(account string) ([]byte, bool)
	Set(account string, key []byte) error
}

type noSecretBackend struct{}

func (noSecretBackend) Get(string) ([]byte, bool) { return nil, false }
func (noSecretBackend) Set(string, []byte) error  { return nil }

// execSecretBackend shells out to `security` (macOS) or `secret-tool` (Linux).
type execSecretBackend struct{ tool string }

func detectSecretBackend() harnessSecretBackend {
	if p, err := exec.LookPath("security"); err == nil {
		return execSecretBackend{tool: p}
	}
	if p, err := exec.LookPath("secret-tool"); err == nil {
		return execSecretBackend{tool: p}
	}
	return noSecretBackend{}
}

// harnessSecretBackendImpl stays nil until the first key lookup detects it.
// Detecting at package init made EVERY solud process -- including each
// `solud agent` spawned per pane -- walk PATH twice for tools that usually
// aren't there. On WSL that PATH carries dozens of /mnt/c dirs, each miss a
// 9P round-trip, which pushed agent startup past the 3s socket dial and
// failed every new pane with a 500. Tests preset it to a fake.
var (
	harnessSecretBackendImpl harnessSecretBackend
	harnessSecretBackendMu   sync.Mutex
)

func currentHarnessSecretBackend() harnessSecretBackend {
	harnessSecretBackendMu.Lock()
	defer harnessSecretBackendMu.Unlock()
	if harnessSecretBackendImpl == nil {
		harnessSecretBackendImpl = detectSecretBackend()
	}
	return harnessSecretBackendImpl
}

func (b execSecretBackend) Get(account string) ([]byte, bool) {
	var out []byte
	var err error
	if filepath.Base(b.tool) == "security" {
		out, err = hiddenCommand(context.Background(), b.tool, "find-generic-password", "-s", "soluterminal-harness", "-a", account, "-w").Output()
	} else {
		out, err = hiddenCommand(context.Background(), b.tool, "lookup", "service", "soluterminal-harness", "account", account).Output()
	}
	if err != nil {
		return nil, false
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(raw) != harnessSecretKeyBytes {
		return nil, false
	}
	return raw, true
}

func (b execSecretBackend) Set(account string, key []byte) error {
	encoded := hex.EncodeToString(key)
	if filepath.Base(b.tool) == "security" {
		return hiddenCommand(context.Background(), b.tool, "add-generic-password", "-s", "soluterminal-harness", "-a", account, "-w", encoded, "-U").Run()
	}
	cmd := hiddenCommand(context.Background(), b.tool, "store", "--label=soluterminal harness credential key", "service", "soluterminal-harness", "account", account)
	cmd.Stdin = strings.NewReader(encoded + "\n")
	return cmd.Run()
}

// harnessSecretKey resolves the store key for dataDir and caches it for the
// process lifetime. The keychain is preferred; its absence (or a stored key
// that cannot be read) falls through to the 0600 key file.
func harnessSecretKey(dataDir string) ([]byte, error) {
	if cached, ok := harnessSecretKeyCache.Load(dataDir); ok {
		return cached.([]byte), nil
	}
	key, err := loadOrCreateHarnessSecretKey(dataDir)
	if err != nil {
		return nil, err
	}
	harnessSecretKeyCache.Store(dataDir, key)
	return key, nil
}

var harnessSecretKeyCache sync.Map // dataDir -> []byte

// harnessSecretAccount is the stable keychain account name for a data dir, so
// two data dirs on one machine never share a key. Abs-path hashing means a
// symlinked or relative spelling of the same dir resolves to one key.
func harnessSecretAccount(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:24]
}

func loadOrCreateHarnessSecretKey(dataDir string) ([]byte, error) {
	account := harnessSecretAccount(dataDir)
	backend := currentHarnessSecretBackend()

	if key, ok := backend.Get(account); ok {
		return key, nil
	}

	keyPath := filepath.Join(dataDir, "harness-accounts", ".secret-key")
	if raw, readErr := os.ReadFile(keyPath); readErr == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != harnessSecretKeyBytes {
			return nil, errors.New("harness credential key file is malformed")
		}
		_ = backend.Set(account, key) // best-effort import into the keychain
		return key, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, errors.New("cannot read harness credential key")
	}

	key := make([]byte, harnessSecretKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// With a keychain, the key lives only there. Without one, persist the 0600
	// key file so the store survives a daemon restart.
	if _, ok := backend.(noSecretBackend); !ok {
		if err := backend.Set(account, key); err != nil {
			return nil, errors.New("could not store the harness credential key in the OS keychain")
		}
		return key, nil
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, err
	}
	if err := writeFileAtomicSynced(keyPath, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return nil, err
	}
	return key, nil
}

// sealHarnessSecret encrypts plain into "enc:v1:<base64(nonce||ciphertext)>".
func sealHarnessSecret(key []byte, plain string) (string, error) {
	gcm, err := newHarnessGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return harnessSecretPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// openHarnessSecret decrypts a sealed value. A value without the prefix is a
// legacy plaintext key and is returned as-is (migrated on the next write).
func openHarnessSecret(key []byte, value string) (string, error) {
	if !strings.HasPrefix(value, harnessSecretPrefix) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, harnessSecretPrefix))
	if err != nil {
		return "", errors.New("harness credential is not valid base64")
	}
	gcm, err := newHarnessGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("harness credential ciphertext is too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("harness credential could not be decrypted (wrong key or tampered store)")
	}
	return string(plain), nil
}

func newHarnessGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != harnessSecretKeyBytes {
		return nil, errors.New("harness credential key has the wrong length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
