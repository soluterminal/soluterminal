//go:build linux || windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

func codexLiveEngineExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ProcessState != nil {
		// ProcessState contains an exit code or signal, never provider output.
		return fmt.Errorf("Codex engine stopped (%s); resume this conversation", exit.ProcessState.String())
	}
	return errors.New("Codex engine stopped; resume this conversation")
}

func codexLiveClosedChannel(serverDone chan error) error {
	if err := codexLivePendingEngineExit(serverDone); err != nil {
		return err
	}
	return errors.New("Codex engine connection closed; resume this conversation")
}

func codexLivePendingEngineExit(serverDone chan error) error {
	// Socket EOF can beat Wait after a signal. Give Wait a bounded opportunity
	// to report the real termination instead of labelling it an auth failure.
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-serverDone:
		serverDone <- err // cleanup still owns reaping the child
		if err == nil {
			return nil
		}
		return codexLiveEngineExit(err)
	case <-timer.C:
		return nil
	}
}
