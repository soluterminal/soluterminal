// Package migration inventories explicit WSL and host fixture directories for
// ST-253. It has no apply operation, daemon route, process control or network IO.
package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxStateBytes = 32 << 20

// Keep this allowlist explicit. In particular, never open daemon identity keys,
// the separate owner token, env files or account credential stores. claim.json
// is itself a pairing credential; its bearer is read but never reported.
var stateFiles = [...]string{"panes.json", "workspaces.json", "trusted-devices.json", "claim.json"}

type FileStamp struct {
	Side   string `json:"side"`
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	SHA256 string `json:"sha256,omitempty"`
}

type Counts struct {
	Panes      int `json:"panes"`
	Workspaces int `json:"workspaces"`
	Devices    int `json:"devices"`
}

type Collision struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Inventory is a preview of inputs, not consent or proof that writers stopped.
// It deliberately contains no commands, cwd, prompts, device keys or bearers.
type Inventory struct {
	Schema      int         `json:"schema"`
	Revision    string      `json:"revision"`
	ApplyReady  bool        `json:"applyReady"`
	Source      Counts      `json:"source"`
	Host        Counts      `json:"host"`
	Files       []FileStamp `json:"files"`
	Collisions  []Collision `json:"collisions"`
	ClaimAction string      `json:"claimAction"`
	// Policies are encoded file values, not resolved runtime defaults.
	SourcePolicy   string   `json:"sourcePolicy"`
	HostPolicy     string   `json:"hostPolicy"`
	RemainingSteps []string `json:"remainingSteps"`
}

type claimMetadata struct {
	MachineID string `json:"machineId"`
	RelayURL  string `json:"relayUrl"`
	Token     string `json:"token"`
}

type snapshot struct {
	files      []FileStamp
	panes      map[string]bool
	workspaces map[string]bool
	devices    int
	policy     string
	claim      claimMetadata
}

// Inspect reads only existing, regular state files under two distinct roots.
// Missing files remain missing. It never creates directories or defaults.
// Until the transfer and security adapters land, ApplyReady is always false.
func Inspect(sourceDir, hostDir string) (Inventory, error) {
	sourcePath, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return Inventory{}, fmt.Errorf("source directory is unavailable")
	}
	hostPath, err := filepath.EvalSymlinks(hostDir)
	if err != nil {
		return Inventory{}, fmt.Errorf("host directory is unavailable")
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return Inventory{}, fmt.Errorf("source directory is unavailable")
	}
	hostPath, err = filepath.Abs(hostPath)
	if err != nil {
		return Inventory{}, fmt.Errorf("host directory is unavailable")
	}
	if within(sourcePath, hostPath) || within(hostPath, sourcePath) {
		return Inventory{}, fmt.Errorf("source and host directories overlap")
	}
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		return Inventory{}, fmt.Errorf("source directory is unavailable")
	}
	defer sourceRoot.Close()
	hostRoot, err := os.OpenRoot(hostPath)
	if err != nil {
		return Inventory{}, fmt.Errorf("host directory is unavailable")
	}
	defer hostRoot.Close()
	sourceInfo, err := sourceRoot.Stat(".")
	if err != nil {
		return Inventory{}, fmt.Errorf("source directory is unavailable")
	}
	hostInfo, err := hostRoot.Stat(".")
	if err != nil {
		return Inventory{}, fmt.Errorf("host directory is unavailable")
	}
	if os.SameFile(sourceInfo, hostInfo) {
		return Inventory{}, fmt.Errorf("source and host directories overlap")
	}
	source, err := readSnapshot(sourceRoot, "source")
	if err != nil {
		return Inventory{}, err
	}
	host, err := readSnapshot(hostRoot, "host")
	if err != nil {
		return Inventory{}, err
	}
	i := Inventory{
		Schema:       1,
		Source:       Counts{len(source.panes), len(source.workspaces), source.devices},
		Host:         Counts{len(host.panes), len(host.workspaces), host.devices},
		Files:        append(source.files, host.files...),
		Collisions:   []Collision{},
		SourcePolicy: source.policy, HostPolicy: host.policy,
		ClaimAction: claimAction(source.claim, host.claim),
		RemainingSteps: []string{
			"security-settings inventory and stricter merge",
			"device intersection and host-key re-approval",
			"Linux pane conversion through the helper",
			"human confirmation, writer stop and claim-mirror barrier",
			"locked transfer, backups and recovery",
			"host cloud liveness and owner-authenticated source retirement",
		},
	}
	for kind, pair := range map[string][2]map[string]bool{
		"pane": {source.panes, host.panes}, "workspace": {source.workspaces, host.workspaces},
	} {
		for id := range pair[0] {
			if pair[1][id] {
				i.Collisions = append(i.Collisions, Collision{kind, id})
			}
		}
	}
	sort.Slice(i.Collisions, func(a, b int) bool {
		if i.Collisions[a].Kind != i.Collisions[b].Kind {
			return i.Collisions[a].Kind < i.Collisions[b].Kind
		}
		return i.Collisions[a].ID < i.Collisions[b].ID
	})
	// Fixed names and sides make the encoded stamps unambiguous. Unknown fields
	// and absent-vs-present files affect this revision, without being rewritten.
	stamps, _ := json.Marshal(i.Files)
	digest := sha256.Sum256(stamps)
	i.Revision = hex.EncodeToString(digest[:])
	return i, nil
}

func within(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type stateRoot interface {
	Lstat(string) (os.FileInfo, error)
	Open(string) (*os.File, error)
}

func readSnapshot(root stateRoot, side string) (snapshot, error) {
	s := snapshot{panes: map[string]bool{}, workspaces: map[string]bool{}, policy: "unspecified"}
	for _, name := range stateFiles {
		data, exists, err := readStateFile(root, name)
		if err != nil {
			return s, fmt.Errorf("%s/%s: %s", side, name, err)
		}
		stamp := FileStamp{Side: side, Name: name, Exists: exists}
		if exists {
			hash := sha256.Sum256(data)
			stamp.SHA256 = hex.EncodeToString(hash[:])
			if validateJSON(data) != nil {
				return s, fmt.Errorf("%s/%s: invalid or ambiguous JSON", side, name)
			}
			if err := parseState(&s, name, data); err != nil {
				return s, fmt.Errorf("%s/%s: %s", side, name, err)
			}
		}
		s.files = append(s.files, stamp)
	}
	return s, nil
}

func readStateFile(root stateRoot, name string) ([]byte, bool, error) {
	before, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cannot inspect file")
	}
	if !before.Mode().IsRegular() {
		return nil, false, fmt.Errorf("not a regular file")
	}
	if before.Size() > maxStateBytes {
		return nil, false, fmt.Errorf("file exceeds inventory limit")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false, fmt.Errorf("cannot open file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, false, fmt.Errorf("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("cannot read file")
	}
	if len(data) > maxStateBytes {
		return nil, false, fmt.Errorf("file exceeds inventory limit")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, false, fmt.Errorf("file changed while reading")
	}
	return data, true, nil
}

func parseState(s *snapshot, name string, data []byte) error {
	switch name {
	case "panes.json":
		var panes []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &panes) != nil {
			return fmt.Errorf("expected a pane array")
		}
		for _, pane := range panes {
			if pane.ID == "" || len(pane.ID) > 4096 || s.panes[pane.ID] {
				return fmt.Errorf("empty, oversized or duplicate pane ID")
			}
			s.panes[pane.ID] = true
		}
	case "workspaces.json":
		// WorkspaceStore.commit persists a list, not its in-memory name map.
		// Names are the persisted workspace identities (workspace.go).
		var workspaces []*struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &workspaces) != nil {
			return fmt.Errorf("expected a workspace array")
		}
		for _, workspace := range workspaces {
			if workspace == nil || workspace.Name == "" || len(workspace.Name) > 4096 || s.workspaces[workspace.Name] {
				return fmt.Errorf("empty, oversized or duplicate workspace name")
			}
			s.workspaces[workspace.Name] = true
		}
	case "trusted-devices.json":
		var record struct {
			Policy  string            `json:"policy"`
			Devices []json.RawMessage `json:"devices"`
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &record) != nil {
			return fmt.Errorf("expected a device record")
		}
		s.devices = len(record.Devices)
		for _, raw := range record.Devices {
			var item map[string]json.RawMessage
			if json.Unmarshal(raw, &item) != nil || item == nil {
				return fmt.Errorf("invalid device record")
			}
		}
		if record.Policy != "" && record.Policy != "any" && record.Policy != "approved" {
			return fmt.Errorf("unsupported device policy")
		}
		if record.Policy != "" {
			s.policy = record.Policy
		}
	case "claim.json":
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &s.claim) != nil {
			return fmt.Errorf("expected a claim record")
		}
		if s.claim.Token != "" && (s.claim.MachineID == "" || s.claim.RelayURL == "") {
			return fmt.Errorf("incomplete claim metadata")
		}
	}
	return nil
}

func claimAction(source, host claimMetadata) string {
	if source.Token == "" {
		return "no-source-claim"
	}
	if host.Token == "" {
		return "host-pairing-required"
	}
	if source.RelayURL != host.RelayURL {
		return "relay-mismatch-needs-decision"
	}
	if source.MachineID == host.MachineID {
		return "stop-mirror-then-rename-source-revoke-nothing"
	}
	return "verify-live-host-stop-source-then-owner-revoke-source"
}

// Reject duplicate names even in unknown objects. A future adapter must not
// interpret a different value from the one the person saw in this inventory.
func validateJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("deep JSON")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate JSON name")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("invalid object")
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("invalid array")
			}
		default:
			return fmt.Errorf("invalid delimiter")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
