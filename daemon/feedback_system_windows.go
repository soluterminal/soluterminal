//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func feedbackWindowsBuildProbe() string {
	v := windows.RtlGetVersion()
	if v == nil || v.MajorVersion == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
