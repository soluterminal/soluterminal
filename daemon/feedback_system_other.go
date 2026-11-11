//go:build !windows

package main

func feedbackWindowsBuildProbe() string {
	if feedbackInWSL() {
		return feedbackWSLWindowsVer()
	}
	return ""
}
