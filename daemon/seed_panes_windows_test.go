//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A seed file written by this user in their own folder loads on Windows; one
// that another group may write is refused before it is decoded.
func TestLoadSeedPanesWindowsOwnerAndACL(t *testing.T) {
	data := t.TempDir()
	path := seedPanesPath(data)
	if err := os.WriteFile(path, []byte(`{"panes":[{"title":"rig","distro":"SoluTerminalRig"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := loadSeedPanes(data)
	if err != nil || len(specs) != 1 || specs[0].Distro != "SoluTerminalRig" {
		t.Fatalf("a private seed file was refused: %v", err)
	}

	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	grant := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_WRITE,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(users),
		},
	}}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries(grant, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSeedPanes(data); err == nil || !strings.Contains(err.Error(), "writable by") {
		t.Fatalf("a seed file Users may write was accepted: %v", err)
	}
}

func TestDaemonRoleSeesDataDirAliasesOnWindows(t *testing.T) {
	home := t.TempDir()
	def := filepath.Join(home, ".solu")
	if err := os.Mkdir(def, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := decideDaemonRole("windows", "127.0.0.1:18797", strings.ToUpper(def), def); !r.DefaultData {
		t.Fatal("a case-changed default -data passed as its own")
	}
}

// The DACL rule on descriptors built from SDDL: only this user, SYSTEM and
// Administrators may be granted write; entries of a type the check doesn't
// read (object, conditional) are refused.
func TestSeedDACLPrivateEntryTypes(t *testing.T) {
	tok, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tok.User.Sid.String()
	for _, tc := range []struct {
		name, dacl string
		ok         bool
	}{
		{"profile default", "(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + me + ")", true},
		{"others may only read", "(A;;FA;;;" + me + ")(A;;FR;;;BU)", true},
		{"a deny entry", "(D;;FW;;;BU)(A;;FA;;;" + me + ")", true},
		{"Users may write", "(A;;FA;;;" + me + ")(A;;FW;;;BU)", false},
		{"Everyone full", "(A;;FA;;;WD)", false},
		{"conditional grant", "(A;;FA;;;" + me + ")(XA;;FW;;;BU;(Member_of {SID(BA)}))", false},
		{"object grant", "(A;;FA;;;" + me + ")(OA;;FW;bf967aba-0de6-11d0-a285-00aa003049e2;;BU)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString("O:" + me + "D:" + tc.dacl)
			if err != nil {
				t.Fatalf("SDDL: %v", err)
			}
			if err := seedDACLPrivate(sd, tok.User.Sid); (err == nil) != tc.ok {
				t.Fatalf("seedDACLPrivate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	// No DACL at all is writable by everyone.
	sd, err := windows.SecurityDescriptorFromString("O:" + me + "D:NO_ACCESS_CONTROL")
	if err != nil {
		t.Fatal(err)
	}
	if seedDACLPrivate(sd, tok.User.Sid) == nil {
		t.Fatal("a NULL DACL was accepted")
	}
}
