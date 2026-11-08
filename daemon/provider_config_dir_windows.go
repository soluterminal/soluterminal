//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Protect at creation, before writing a byte. Chmod(0600) does not restrict a
// Windows ACL. A protected DACL grants only this process's user, with object and
// container inheritance so every JSON file is private too. No inherited Users,
// Administrators or Everyone ACEs from a potentially shared temp directory.
func newPrivateProviderConfigDir() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	// Explicit ownership also keeps elevated launches owned by this user,
	// rather than the token's default Administrators owner.
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return "", err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for range 10 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		path := filepath.Join(os.TempDir(), providerConfigPrefix+hex.EncodeToString(nonce[:]))
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", err
		}
		if err := windows.CreateDirectory(ptr, &sa); errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		} else if err != nil {
			return "", err
		}
		return path, nil
	}
	return "", os.ErrExist
}
