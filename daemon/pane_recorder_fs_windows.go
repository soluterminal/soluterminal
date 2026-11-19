//go:build windows

package main

import (
	"errors"
	"os"
)

// Windows-side daemons refuse the recorder switch (recordingPlatformSupported)
// until the security owner has reviewed Windows file permissions, so these
// refuse too.
var errRecordingUnsupported = errors.New("recording is not available on Windows yet")

func openNewRecordingFile(string) (*os.File, error)      { return nil, errRecordingUnsupported }
func openExistingRecordingFile(string) (*os.File, error) { return nil, errRecordingUnsupported }
func recordingOwnedByUs(os.FileInfo) bool                { return false }
