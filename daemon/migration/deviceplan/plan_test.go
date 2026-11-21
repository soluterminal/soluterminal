package deviceplan

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixtureKey(t *testing.T, curve elliptic.Curve, scalar byte) string {
	t.Helper()
	x, y := curve.ScalarBaseMult([]byte{scalar})
	der, err := x509.MarshalPKIXPublicKey(&ecdsa.PublicKey{Curve: curve, X: x, Y: y})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func row(key, name, status, scope string) map[string]any {
	return map[string]any{"publicKey": key, "name": name, "status": status, "scope": scope}
}

// Fixtures use trusted-devices.json's real object/array shape. EffectivePolicy
// is supplied explicitly by the test, as a later authenticated adapter must do.
func fixtureInput(t *testing.T, policy Policy, present bool, rows ...map[string]any) Input {
	t.Helper()
	dir := t.TempDir()
	i := Input{ListPresent: present, EffectivePolicy: policy}
	if !present {
		return i
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	data, err := json.Marshal(map[string]any{"policy": policy, "devices": rows})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trusted-devices.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	i.Devices = decoded.Devices
	return i
}

func TestPlanIntersectsByKeyAndRequiresFreshHostApproval(t *testing.T) {
	k1, k2, k3 := fixtureKey(t, elliptic.P256(), 1), fixtureKey(t, elliptic.P256(), 2), fixtureKey(t, elliptic.P256(), 3)
	s := row(k1, "Old name", "approved", "")
	s["id"] = "source-id-is-not-identity"
	s["v3Verified"] = map[string]any{"daemonSPKIHash": "OLD-PRIVATE-PROOF"}
	s["daemonNonce"] = "OLD-PRIVATE-NONCE"
	source := fixtureInput(t, Any, true, s, row(k2, "Same display name", "approved", "full"))
	host := fixtureInput(t, ApprovedOnly, true, row(k1, "Host name", "approved", "full"), row(k3, "Same display name", "approved", "full"))
	p, err := Build(source, host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Policy != ApprovedOnly || p.ApplyReady || len(p.Candidates) != 1 || len(p.NotCarried) != 2 {
		t.Fatalf("unexpected plan: %+v", p)
	}
	c := p.Candidates[0]
	if c.Name != "Host name" || c.Scope != Full || c.From != "both" || !c.RequiresHostApproval || len(c.Fingerprint) != 64 {
		t.Fatalf("candidate = %+v", c)
	}
	for _, n := range p.NotCarried {
		if n.Reason != "not-in-both-lists" || !n.CanRequestApproval {
			t.Fatalf("notice = %+v", n)
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{k1, k2, k3, "publicKey", "v3Verified", "OLD-PRIVATE-", "daemonNonce", "source-id-is-not-identity"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("plan disclosed input field %q", forbidden)
		}
	}
}

func TestPlanDistinguishesAbsentAndPresentEmptyLists(t *testing.T) {
	key := fixtureKey(t, elliptic.P256(), 1)
	source := fixtureInput(t, Any, true, row(key, "Phone", "approved", ""))
	for _, tc := range []struct {
		name                string
		host                Input
		candidates, notices int
	}{
		{"absent", fixtureInput(t, ApprovedOnly, false), 1, 0},
		{"present-empty", fixtureInput(t, ApprovedOnly, true), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Build(source, tc.host)
			if err != nil || len(p.Candidates) != tc.candidates || len(p.NotCarried) != tc.notices || p.Policy != ApprovedOnly {
				t.Fatalf("plan = %+v, %v", p, err)
			}
		})
	}
	p, err := Build(fixtureInput(t, Any, false), source)
	if err != nil || len(p.Candidates) != 1 || p.Candidates[0].From != "host" || !p.Candidates[0].RequiresHostApproval {
		t.Fatalf("host-only plan = %+v, %v", p, err)
	}
}

func TestPlanNarrowsScopeIncludingLegacyAlertsStatus(t *testing.T) {
	key := fixtureKey(t, elliptic.P256(), 1)
	for _, tc := range []struct {
		sourceStatus, sourceScope, hostStatus, hostScope string
		want                                             Scope
	}{
		{"approved", "", "approved", "full", Full},
		{"approved", "full", "approved", "", Full},
		{"approved-alerts", "", "approved", "full", Alerts},
		{"approved-alerts", "full", "approved", "", Alerts},
		{"approved", "alerts", "approved", "full", Alerts},
		{"approved", "full", "approved-alerts", "", Alerts},
	} {
		source := fixtureInput(t, Any, true, row(key, "Source", tc.sourceStatus, tc.sourceScope))
		host := fixtureInput(t, Any, true, row(key, "Host", tc.hostStatus, tc.hostScope))
		p, err := Build(source, host)
		if err != nil || len(p.Candidates) != 1 || p.Candidates[0].Scope != tc.want || p.Policy != Any {
			t.Fatalf("scope plan = %+v, %v", p, err)
		}
	}
}

func TestPlanDoesNotCarryUnfinishedStates(t *testing.T) {
	key := fixtureKey(t, elliptic.P256(), 1)
	for _, status := range []string{"pending", "committed", "failed"} {
		source := fixtureInput(t, Any, true, row(key, "Request", status, ""))
		host := fixtureInput(t, Any, true, row(key, "Host", "approved", ""))
		p, err := Build(source, host)
		if err != nil || len(p.Candidates) != 0 || len(p.NotCarried) != 1 {
			t.Fatalf("plan = %+v, %v", p, err)
		}
		if p.NotCarried[0].Reason != "unfinished" || p.NotCarried[0].CanRequestApproval {
			t.Fatalf("unfinished request was offered: %+v", p)
		}
	}
	source := fixtureInput(t, Any, true, row("", "Pending", "pending", ""), row("not-a-key", "Committed", "committed", "alerts"), row("", "Failed", "failed", ""))
	p, err := Build(source, fixtureInput(t, Any, false))
	if err != nil || len(p.Candidates) != 0 || len(p.NotCarried) != 3 {
		t.Fatalf("unfinished missing-key plan = %+v, %v", p, err)
	}
	for index, n := range p.NotCarried {
		if n.Fingerprint != "" || n.RecordIndex != index+1 || n.Reason != "unfinished" || n.CanRequestApproval {
			t.Fatalf("notice = %+v", n)
		}
	}
}

func TestPlanUnknownSemanticsAbortEvenForExcludedRecords(t *testing.T) {
	key := fixtureKey(t, elliptic.P256(), 1)
	for _, r := range []map[string]any{
		row(key, "Unknown status", "future-status", "full"),
		row(key, "Unknown scope", "approved", "view"),
		row("", "Unfinished unknown scope", "pending", "future-scope"),
		row(key, "Missing status", "", ""),
	} {
		source := fixtureInput(t, Any, true, r)
		if _, err := Build(source, fixtureInput(t, Any, true)); err == nil {
			t.Fatal("unknown source semantics were silently dropped")
		}
		if _, err := Build(fixtureInput(t, Any, false), source); err == nil {
			t.Fatal("unknown host semantics were silently dropped")
		}
	}
}

func TestPlanCanonicalizesKeyIdentityAndRejectsDuplicates(t *testing.T) {
	key := fixtureKey(t, elliptic.P256(), 1)
	wrapped := " \n" + key[:40] + "\n" + key[40:] + "\n "
	source := fixtureInput(t, Any, true, row(wrapped, "Source", "approved", ""))
	host := fixtureInput(t, Any, true, row(key, "Host", "approved", ""))
	p, err := Build(source, host)
	if err != nil || len(p.Candidates) != 1 {
		t.Fatalf("same key did not intersect: %+v, %v", p, err)
	}
	source = fixtureInput(t, Any, true, row(key, "First", "approved", ""), row(wrapped, "Second", "pending", ""))
	if _, err := Build(source, host); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key error = %v", err)
	}
}

func TestPlanRejectsUnsupportedApprovedPublicKeysWithoutDisclosure(t *testing.T) {
	for _, key := range []string{"", "PRIVATE-INVALID-KEY", fixtureKey(t, elliptic.P384(), 1)} {
		source := fixtureInput(t, Any, true, row(key, "Private label", "approved", ""))
		_, err := Build(source, fixtureInput(t, Any, false))
		if err == nil || strings.Contains(err.Error(), "PRIVATE-") || strings.Contains(err.Error(), "Private label") {
			t.Fatalf("key error = %v", err)
		}
	}
}

func TestPlanRequiresResolvedPoliciesAndConsistentPresence(t *testing.T) {
	source, host := fixtureInput(t, Any, false), fixtureInput(t, ApprovedOnly, false)
	p, err := Build(source, host)
	if err != nil || p.Policy != ApprovedOnly || len(p.Candidates) != 0 {
		t.Fatalf("empty plan = %+v, %v", p, err)
	}
	for _, policy := range []Policy{"", "unspecified", "future-policy"} {
		source.EffectivePolicy = policy
		if _, err := Build(source, host); err == nil {
			t.Fatal("unresolved policy was guessed")
		}
	}
	source = fixtureInput(t, Any, true, row(fixtureKey(t, elliptic.P256(), 1), "Phone", "approved", ""))
	source.ListPresent = false
	if _, err := Build(source, host); err == nil {
		t.Fatal("records in absent list accepted")
	}
}

func TestPlanIsPureAndDeterministic(t *testing.T) {
	k1, k2 := fixtureKey(t, elliptic.P256(), 1), fixtureKey(t, elliptic.P256(), 2)
	source := fixtureInput(t, Any, true, row(k1, "One", "approved", ""), row(k2, "Two", "approved", "alerts"))
	host := fixtureInput(t, ApprovedOnly, true, row(k2, "Two host", "approved", ""), row(k1, "One host", "approved", ""))
	sourceBefore, hostBefore := append([]Device(nil), source.Devices...), append([]Device(nil), host.Devices...)
	first, err := Build(source, host)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(source, host)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("unstable plan")
	}
	if !reflect.DeepEqual(source.Devices, sourceBefore) || !reflect.DeepEqual(host.Devices, hostBefore) {
		t.Fatal("inputs were modified")
	}
	source.Devices[0], source.Devices[1] = source.Devices[1], source.Devices[0]
	third, err := Build(source, host)
	if err != nil || !reflect.DeepEqual(first, third) {
		t.Fatal("record ordering changed membership plan")
	}
}
