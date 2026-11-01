//go:build !linux && !windows

package main

import "errors"

func codexLiveSupported() bool            { return false }
func codexLivePaneHasBridge(p *Pane) bool { return false }
func codexLiveSpawnCommand(self, dir, daemonURL string, cmd []string, routeOverrides ...[]string) ([]string, error) {
	return nil, errors.New("Codex live authentication requires Linux")
}
func runCodexTerminal() int { return 2 }

func (p *Pane) codexLiveInputLease(b []byte) (func(), error) { return func() {}, nil }
