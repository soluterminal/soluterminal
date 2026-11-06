//go:build !windows

package main

func providerBatchArgv(bin string, args, env []string) (string, []string, error) {
	return bin, args, nil
}
