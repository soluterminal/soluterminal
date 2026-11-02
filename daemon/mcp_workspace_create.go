package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

const workspaceCreateHourlyLimit = 5
const workspaceCreateGlobalHourlyLimit = 20

type workspaceCreateLimiter struct {
	mu      sync.Mutex
	created map[string][]time.Time
}

func (a *apiServer) mcpToolCreateWorkspace(args map[string]any, caller paneCaller) any {
	if a.ws == nil {
		return toolErr("workspace store not initialized")
	}
	name := strings.TrimSpace(mcpArgString(args, "name"))
	bucket := strings.TrimSpace(caller.ID)
	if bucket == "" || a.mgr == nil {
		return toolErr("create_workspace requires a verified connection from a running SoluTerminal pane; detached, worker and unverified callers cannot create workspaces. Use the app if this connection cannot be traced.")
	}
	pane, found := a.mgr.Get(bucket)
	if !found || pane.Snapshot().Status != PaneRunning {
		return toolErr("create_workspace requires a verified connection from a running SoluTerminal pane")
	}
	limiter := &a.workspaceCreates
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := time.Now()
	if limiter.created == nil {
		limiter.created = map[string][]time.Time{}
	}
	total := 0
	for key, timestamps := range limiter.created {
		kept := timestamps[:0]
		for _, ts := range timestamps {
			if now.Sub(ts) < time.Hour {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(limiter.created, key)
		} else {
			limiter.created[key] = kept
			total += len(kept)
		}
	}
	if len(limiter.created[bucket]) >= workspaceCreateHourlyLimit {
		return toolErr("create_workspace is limited to 5 new workspaces per pane per hour; use an existing workspace or wait")
	}
	if total >= workspaceCreateGlobalHourlyLimit {
		return toolErr("create_workspace is limited to 20 new workspaces per hour across all panes; use an existing workspace or wait")
	}
	workspace, err := a.ws.CreateEmpty(name)
	if err != nil {
		return toolErr(err.Error())
	}
	limiter.created[bucket] = append(limiter.created[bucket], now)
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "workspace.updated", Workspace: workspace.Name})
	}
	data, _ := json.Marshal(workspace)
	return toolOK(string(data))
}
