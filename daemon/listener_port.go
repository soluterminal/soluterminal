package main

import (
	"fmt"
	"net"
)

func listenOnConfiguredPort(network, addr string) (net.Listener, error) {
	// Never enable SO_REUSEPORT here: Shinoque's core proves which process
	// answers this port by the pid holding its only listening socket.
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return retainConfiguredPort(ln, addr)
}

func retainConfiguredPort(ln net.Listener, requested string) (net.Listener, error) {
	_, port, err := net.SplitHostPort(requested)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("invalid daemon listener address %q: %w", requested, err)
	}
	expected, err := net.LookupPort("tcp", port)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	actual, ok := ln.Addr().(*net.TCPAddr)
	// Some WSL host forwarding conflicts silently turn a fixed-port bind into
	// an ephemeral listener. That cannot prove ownership of the daemon port or
	// authorize an update handoff and a second writer to the same durable state.
	if !ok || (expected != 0 && actual.Port != expected) {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon listener requested %s but bound %s; refusing startup", requested, ln.Addr())
	}
	return ln, nil
}
