// Package deviceplan prepares re-approval metadata for guided ST-253 migration.
// It never produces an approved device record or performs IO.
package deviceplan

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type Policy string

const (
	Any          Policy = "any"
	ApprovedOnly Policy = "approved"
)

type Scope string

const (
	Full   Scope = "full"
	Alerts Scope = "alerts"
)

// Device contains only membership metadata. Approval proofs and daemon keys
// cannot be passed through this type into the plan.
type Device struct {
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	Status    string `json:"status"`
	Scope     string `json:"scope"`
}

// Input requires a resolved policy even for a missing file. Missing and empty
// lists have different intersection semantics. The caller must strictly parse
// and bind records and effective defaults; an inventory's "unspecified" value
// is intentionally not accepted as an effective policy.
type Input struct {
	ListPresent     bool
	EffectivePolicy Policy
	Devices         []Device
}

type Candidate struct {
	Fingerprint          string `json:"fingerprint"`
	Name                 string `json:"name"`
	Scope                Scope  `json:"scope"`
	From                 string `json:"from"`
	RequiresHostApproval bool   `json:"requiresHostApproval"`
}

type Notice struct {
	Fingerprint        string `json:"fingerprint,omitempty"`
	Name               string `json:"name"`
	Scope              Scope  `json:"scope"`
	From               string `json:"from"`
	RecordIndex        int    `json:"recordIndex,omitempty"`
	Reason             string `json:"reason"`
	CanRequestApproval bool   `json:"canRequestApproval"`
}

// Plan cannot be serialized as trusted-devices.json: it contains neither keys,
// statuses nor proofs. A candidate still needs the device's participation in a
// fresh host-key approval. Approval history/bootstrap controls are not planned
// here and must be preserved by the later transfer adapter.
type Plan struct {
	Policy     Policy      `json:"policy"`
	ApplyReady bool        `json:"applyReady"`
	Candidates []Candidate `json:"candidates"`
	NotCarried []Notice    `json:"notCarried"`
}

type record struct {
	name   string
	scope  Scope
	active bool
}

// Build applies ruling 9's membership rules. It is pure, does not modify the
// inputs, and always returns ApplyReady=false. Unknown security semantics fail
// the entire plan, even for records that would otherwise be excluded.
func Build(source, host Input) (Plan, error) {
	for side, input := range map[string]Input{"source": source, "host": host} {
		if input.EffectivePolicy != Any && input.EffectivePolicy != ApprovedOnly {
			return Plan{}, fmt.Errorf("%s: effective access policy must be explicit and supported", side)
		}
		if !input.ListPresent && len(input.Devices) != 0 {
			return Plan{}, fmt.Errorf("%s: absent list contains device records", side)
		}
	}
	sourceRecords, sourceNotices, err := collect(source.Devices, "source")
	if err != nil {
		return Plan{}, err
	}
	hostRecords, hostNotices, err := collect(host.Devices, "host")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Policy: Any, Candidates: []Candidate{}, NotCarried: append(sourceNotices, hostNotices...)}
	if p.NotCarried == nil {
		p.NotCarried = []Notice{}
	}
	if source.EffectivePolicy == ApprovedOnly || host.EffectivePolicy == ApprovedOnly {
		p.Policy = ApprovedOnly
	}
	keys := map[string]bool{}
	for key := range sourceRecords {
		keys[key] = true
	}
	for key := range hostRecords {
		keys[key] = true
	}
	for key := range keys {
		s, inSource := sourceRecords[key]
		h, inHost := hostRecords[key]
		chosen, from := s, "source"
		if inHost {
			chosen, from = h, "host"
		}
		if inSource && inHost {
			from = "both"
			if s.scope == Alerts || h.scope == Alerts {
				chosen.scope = Alerts
			}
		}
		if (inSource && !s.active) || (inHost && !h.active) {
			p.NotCarried = append(p.NotCarried, Notice{Fingerprint: key, Name: chosen.name, Scope: chosen.scope, From: from, Reason: "unfinished"})
			continue
		}
		if source.ListPresent && host.ListPresent && (!inSource || !inHost) {
			p.NotCarried = append(p.NotCarried, Notice{Fingerprint: key, Name: chosen.name, Scope: chosen.scope, From: from, Reason: "not-in-both-lists", CanRequestApproval: true})
			continue
		}
		p.Candidates = append(p.Candidates, Candidate{Fingerprint: key, Name: chosen.name, Scope: chosen.scope, From: from, RequiresHostApproval: true})
	}
	sort.Slice(p.Candidates, func(i, j int) bool { return p.Candidates[i].Fingerprint < p.Candidates[j].Fingerprint })
	sort.Slice(p.NotCarried, func(i, j int) bool {
		a, b := p.NotCarried[i], p.NotCarried[j]
		if a.Fingerprint != b.Fingerprint {
			return a.Fingerprint < b.Fingerprint
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.RecordIndex < b.RecordIndex
	})
	return p, nil
}

func collect(devices []Device, side string) (map[string]record, []Notice, error) {
	records, notices := map[string]record{}, []Notice{}
	for index, d := range devices {
		active := false
		switch d.Status {
		case "approved", "approved-alerts":
			active = true
		case "pending", "committed", "failed":
		default:
			return nil, nil, fmt.Errorf("%s record %d: unsupported device status", side, index+1)
		}
		var scope Scope
		switch d.Scope {
		case "", "full":
			scope = Full
		case "alerts":
			scope = Alerts
		default:
			return nil, nil, fmt.Errorf("%s record %d: unsupported device scope", side, index+1)
		}
		if d.Status == "approved-alerts" {
			scope = Alerts
		}
		key, err := fingerprint(d.PublicKey)
		if err != nil {
			if active {
				return nil, nil, fmt.Errorf("%s record %d: invalid device public key", side, index+1)
			}
			// Unfinished records are never carried. They may have no usable
			// key; their status/scope must still be recognized above.
			notices = append(notices, Notice{Name: d.Name, Scope: scope, From: side, RecordIndex: index + 1, Reason: "unfinished"})
			continue
		}
		if _, duplicate := records[key]; duplicate {
			return nil, nil, fmt.Errorf("%s record %d: duplicate device public key", side, index+1)
		}
		records[key] = record{name: d.Name, scope: scope, active: active}
	}
	return records, notices, nil
}

func fingerprint(encoded string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", err
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return "", fmt.Errorf("unsupported key")
	}
	canonical, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
