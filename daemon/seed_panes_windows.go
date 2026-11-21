//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// seedWriteRights are the rights that would let someone change the seed file
// or its protection.
const seedWriteRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
	windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_WRITE | windows.GENERIC_ALL

// seedFileOwnedPrivately is the Windows form of "owned by this user and not
// writable by group or others", read from the open file's handle: this user
// owns it, and its DACL lets only this user, SYSTEM and Administrators (the
// owner-equivalents of a profile file) write it. A missing (NULL) DACL is
// writable by everyone and refused.
func seedFileOwnedPrivately(_ fs.FileInfo, f *os.File) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cannot read the file's owner and access list: %w", err)
	}
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("cannot read this user: %w", err)
	}
	owner, _, err := sd.Owner()
	def := seedDefaultOwner()
	if err != nil || owner == nil || !(owner.Equals(token.User.Sid) || (def != nil && owner.Equals(def))) {
		return errors.New("not owned by this user")
	}
	return seedDACLPrivate(sd, token.User.Sid)
}

// seedDefaultOwner is the owner this process gives the files it creates: the
// user, or Administrators for an elevated administrator. nil when unreadable.
func seedDefaultOwner() *windows.SID {
	token := windows.GetCurrentProcessToken()
	var n uint32
	_ = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &n)
	if n == 0 || n > 4096 {
		return nil
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], n, &n); err != nil {
		return nil
	}
	owner := (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner
	if owner == nil || !owner.IsValid() {
		return nil
	}
	kept, err := owner.Copy()
	if err != nil {
		return nil
	}
	return kept
}

func seedDACLPrivate(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("it has no access list, so anyone may write it")
	}
	trusted := []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("cannot read its access list: %w", err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue // a deny entry only takes rights away
		default:
			// Object, callback and conditional entries can grant too, and
			// this check doesn't read them: refuse rather than guess.
			return fmt.Errorf("its access list has an entry of type %d this check doesn't read", ace.Header.AceType)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&seedWriteRights == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user) || isWellKnownAny(sid, trusted) {
			continue
		}
		name := sid.String()
		return fmt.Errorf("writable by %s, not only this user", name)
	}
	return nil
}

func isWellKnownAny(sid *windows.SID, types []windows.WELL_KNOWN_SID_TYPE) bool {
	for _, t := range types {
		if sid.IsWellKnown(t) {
			return true
		}
	}
	return false
}
