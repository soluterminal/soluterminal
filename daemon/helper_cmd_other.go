//go:build !linux

package main

import (
	"fmt"
	"os"
)

func runHelperCommand([]string) int {
	fmt.Fprintln(os.Stderr, "solud helper runs only inside a Linux (WSL) distro")
	return 2
}
