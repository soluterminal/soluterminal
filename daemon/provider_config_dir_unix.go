//go:build !windows

package main

import "os"

func newPrivateProviderConfigDir() (string, error) {
	return os.MkdirTemp("", "solu-claude-config-") // created as 0700
}
