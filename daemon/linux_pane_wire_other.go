//go:build !windows

package main

import "errors"

// Linux panes through a helper exist only on a Windows host.
func helperHostInputsForThisHost() (helperHostInputs, error) {
	return helperHostInputs{}, errors.New("Linux panes through a WSL helper run only on a Windows host")
}
