package main

import (
	"errors"
	"fmt"
	"strings"
)

// Both the UI's REST route and MCP use this validation, atomic store mutation
// and live event publication. No process or pane identity changes on a move.
var errWorkspacePaneNotFound = errors.New("pane not found")

type workspacePaneMoveResult struct {
	Source      *Workspace `json:"source"`
	Destination *Workspace `json:"destination"`
}

func (a *apiServer) moveWorkspacePaneNow(source, destination, paneID, direction, target string, version uint64) (*workspacePaneMoveResult, error) {
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return nil, errWorkspacePaneNotFound
	}
	snap := p.Snapshot()
	if snap.Role == paneRoleTaskOwner || snap.Status != PaneRunning {
		return nil, fmt.Errorf("only running ordinary panes can be moved")
	}
	src, dst, err := a.ws.MovePane(source, destination, paneID, direction, target, version)
	if err != nil {
		return nil, err
	}
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "workspace.pane-moved", Workspace: source, Text: destination, PaneID: paneID})
	}
	return &workspacePaneMoveResult{Source: src, Destination: dst}, nil
}

func (a *apiServer) renameWorkspaceNow(old, name string) error {
	if err := a.ws.Rename(old, name); err != nil {
		return err
	}
	if a.uiBus != nil && old != name {
		a.uiBus.publish(uiEvent{Type: "workspace.renamed", Workspace: old, Text: name})
	}
	return nil
}

func (a *apiServer) deleteWorkspaceNow(name string, emptyOnly bool) error {
	var err error
	if emptyOnly {
		err = a.ws.DeleteEmpty(name)
	} else {
		err = a.ws.Delete(name)
	}
	if err != nil {
		return err
	}
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "workspace.deleted", Workspace: name})
	}
	return nil
}

func (a *apiServer) existingMCPWorkspace(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("workspace is required")
	}
	if strings.EqualFold(name, "_current") {
		return "", fmt.Errorf("_current is hidden; choose a visible named workspace")
	}
	if a.ws == nil {
		return "", fmt.Errorf("workspace store not initialized")
	}
	resolved, ok := resolveWorkspaceName(a.ws, name)
	if !ok {
		return "", fmt.Errorf("no workspace named %q; call list_workspaces for existing names (no workspace is created)", name)
	}
	if err := validateWorkspaceConfirmName(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// Existing names can predate today's validation or have been written by the
// UI. Refuse unsafe identifiers rather than display a different name in Allow.
func validateWorkspaceConfirmName(name string) error {
	if name != printableText(name) {
		return fmt.Errorf("workspace name contains control or bidi characters; rename it in the app before using agent workspace tools")
	}
	return nil
}

func (a *apiServer) mcpToolMovePaneToWorkspace(args map[string]any, c paneCaller) any {
	id := mcpArgString(args, "pane_id")
	if id == "" {
		return toolErr("pane_id is required")
	}
	destination, err := a.existingMCPWorkspace(mcpArgString(args, "workspace"))
	if err != nil {
		return toolErr(err.Error())
	}
	p, ok := a.mgr.Get(id)
	if !ok {
		return toolErr("pane not found; call list_panes")
	}
	source := a.ws.NameForPane(id)
	src, ok := a.ws.Get(source)
	if !ok || source == "" || strings.EqualFold(source, "_current") {
		return toolErr("save this pane in a visible named workspace before moving it")
	}
	if err := validateWorkspaceConfirmName(source); err != nil {
		return toolErr(err.Error())
	}
	if source == destination {
		return toolErr("source and destination workspaces must differ")
	}
	snap := p.Snapshot()
	if snap.Role == paneRoleTaskOwner || snap.Status != PaneRunning {
		return toolErr("only running ordinary panes can be moved")
	}
	run := func() map[string]any {
		if _, err := a.moveWorkspacePaneNow(source, destination, id, "horizontal", "", src.PaneMoveVersion); err != nil {
			return toolErr(err.Error())
		}
		return toolOK(fmt.Sprintf("Moved pane %s from %q to %q; its process keeps running.", id, source, destination))
	}
	// Ownership is the same as paneAgentAuthority; standing Agent permissions
	// deliberately cannot turn the person's pane into an agent-owned one (#888).
	owns := c.ID != "" && (c.ID == id || paneStartedByCaller(snap.Origin, c.ID))
	if owns {
		return run()
	}
	asked, _ := args["user_requested"].(bool)
	if !asked {
		return toolErr("this pane was not started by you; moving it requires user_requested: true and the person's Allow in the app")
	}
	name := printableText(oneLine(a.paneConfirmTarget(p)))
	return a.confirmWorkspaceAction(c, "move-pane", id, name, destination,
		"Move "+name+"?", fmt.Sprintf("%s wants to move %s from workspace %q to workspace %q. Its process keeps running. Choose Allow in the app.", a.paneConfirmCaller(c), name, source, destination), run)
}

func (a *apiServer) mcpToolChangeWorkspace(tool string, args map[string]any, c paneCaller) any {
	workspace, err := a.existingMCPWorkspace(mcpArgString(args, "workspace"))
	if err != nil {
		return toolErr(err.Error())
	}
	asked, _ := args["user_requested"].(bool)
	if !asked {
		return toolErr(tool + " requires user_requested: true and the person's Allow in the app; standing Agent permissions do not bypass Allow")
	}
	var title, message, change string
	var run func() map[string]any
	if tool == "rename_workspace" {
		change = strings.TrimSpace(mcpArgString(args, "new_name"))
		if change == "" || change != printableText(change) || len([]rune(change)) > 120 {
			return toolErr("new_name must be 1–120 printable characters")
		}
		if strings.EqualFold(change, "_current") {
			return toolErr("_current is reserved")
		}
		if found, ok := resolveWorkspaceName(a.ws, change); ok && found != workspace {
			return toolErr("workspace name already exists (case-insensitive)")
		}
		title = fmt.Sprintf("Rename workspace %q to %q?", workspace, change)
		message = fmt.Sprintf("%s wants to rename workspace %q → %q. Choose Allow in the app.", a.paneConfirmCaller(c), workspace, change)
		run = func() map[string]any {
			if err := a.renameWorkspaceNow(workspace, change); err != nil {
				return toolErr(err.Error())
			}
			return toolOK(fmt.Sprintf("Renamed workspace %q to %q.", workspace, change))
		}
	} else {
		if err := a.ws.CheckDeleteEmpty(workspace); err != nil {
			return toolErr(err.Error())
		}
		title = fmt.Sprintf("Delete workspace %q?", workspace)
		message = fmt.Sprintf("%s wants to delete empty workspace %q. Its saved layout, browser/file tiles and context-pack default will be removed; files are not deleted. Choose Allow in the app.", a.paneConfirmCaller(c), workspace)
		run = func() map[string]any {
			if err := a.deleteWorkspaceNow(workspace, true); err != nil {
				return toolErr(err.Error())
			}
			return toolOK(fmt.Sprintf("Deleted empty workspace %q.", workspace))
		}
	}
	return a.confirmWorkspaceAction(c, tool, "", workspace, change, title, message, run)
}

// Workspace operations use the same queue, human-only answer endpoint, TTL,
// deduplication and per-caller limit as pane confirmations. No authority grant
// can answer this queue on the person's behalf.
func (a *apiServer) confirmWorkspaceAction(c paneCaller, action, paneID, name, change, title, message string, run func() map[string]any) any {
	req, err := a.requestPaneConfirmFor(strings.Join([]string{action, c.bucket(), paneID, name, change}, "\x00"), c.bucket(), paneConfirmView{
		Action: action, CallerPaneID: c.ID, Caller: printableText(oneLine(a.paneConfirmCaller(c))), PaneID: paneID,
		Pane: printableText(oneLine(name)), Title: printableText(oneLine(title)), Message: printableText(oneLine(message)),
	}, run)
	if err != nil {
		return toolErr(err.Error())
	}
	return awaitPaneConfirm(req, strings.Replace(paneConfirmWaitingText("The workspace change has not happened yet"), "check list_panes later", "check list_workspaces and list_panes later", 1))
}
