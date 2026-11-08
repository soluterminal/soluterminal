//go:build !windows

package main

import "os"

func newPrivateProviderConfigDir() (string, error) {
	return os.MkdirTemp("", providerConfigPrefix) // created as 0700
}
