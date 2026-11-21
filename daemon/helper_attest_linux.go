//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"golang.org/x/sys/unix"
)

// soPeerPidfd is SO_PEERPIDFD (Linux 6.5+; asm-generic value, which x86_64
// uses). golang.org/x/sys v0.50.0 does not export it.
const soPeerPidfd = 77

// helperPeerOfPlatform reads SO_PEERCRED and SO_PEERPIDFD on c. The pidfd
// refers to the very process that connected, whatever later happens to its
// numeric pid: while signal 0 through it succeeds, that pid has not been
// given to another process, so /proc/<pid> read before that check described
// the connector itself. A kernel without SO_PEERPIDFD is refused: there is
// then no way to bind the pid to the connector.
func helperPeerOfPlatform(c net.Conn) (helperPeer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return helperPeer{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return helperPeer{}, err
	}
	var cred *unix.Ucred
	var credErr, fdErr error
	pidfd := -1
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if credErr == nil {
			pidfd, fdErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, soPeerPidfd)
		}
	}); err != nil {
		return helperPeer{}, err
	}
	if credErr != nil {
		return helperPeer{}, credErr
	}
	if fdErr != nil {
		return helperPeer{}, fmt.Errorf("SO_PEERPIDFD (needs Linux 6.5 or later): %w", fdErr)
	}
	unix.CloseOnExec(pidfd)
	h := &helperPidfd{fd: pidfd}
	return helperPeer{UID: int(cred.Uid), PID: int(cred.Pid), alive: h.alive, close: h.close}, nil
}

// helperPidfd is one peer's pidfd. alive is called per message from several
// goroutines, close once when the connection ends.
type helperPidfd struct {
	mu sync.Mutex
	fd int // -1 once closed
}

func (h *helperPidfd) alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fd >= 0 && unix.PidfdSendSignal(h.fd, 0, nil, 0) == nil
}

func (h *helperPidfd) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fd >= 0 {
		_ = unix.Close(h.fd)
		h.fd = -1
	}
}
