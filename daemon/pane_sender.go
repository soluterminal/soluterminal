package main

import (
	"fmt"
	"strings"
)

// The daemon-observed caller identifies the sender; claims are unverified.
// Never accept a label supplied in tool arguments, and never label raw keys
// or short menu answers.
func (a *apiServer) labelPaneMessage(body, caller, target string) string {
	return a.labelPaneMessageAs(body, splitMCPCaller(caller), target)
}

func (a *apiServer) labelPaneMessageAs(body string, c paneCaller, target string) string {
	// Slash commands, shell escapes and memory shortcuts need their prefix
	// at column zero. They are control input, rather than attributed chat.
	if strings.HasPrefix(body, "/") || strings.HasPrefix(body, "!") || strings.HasPrefix(body, "#") {
		return body
	}
	// Attribution is informational, never an authorization signal. Labels
	// quoted inside the body are ordinary untrusted message content.
	if sender := a.paneMessageSenderAs(c, target); sender != "" {
		return "[from " + sender + "] " + body
	}
	return body
}

func (a *apiServer) paneMessageSender(caller, target string) string {
	return a.paneMessageSenderAs(splitMCPCaller(caller), target)
}

func (a *apiServer) paneMessageSenderAs(c paneCaller, target string) string {
	if c.Claimed != "" {
		// Resolve the label for display only. It grants no ownership, even
		// when the claimed pane is the target or no longer exists.
		if label := a.paneMessageSenderAs(paneCaller{ID: c.Claimed}, ""); label != "" {
			return label + " (unverified)"
		}
		return "an unverified agent"
	}
	caller := c.ID
	if caller == "" || caller == target {
		return ""
	}
	p, ok := a.mgr.Get(caller)
	if !ok {
		return ""
	}
	snap := p.Snapshot()
	agent := paneSummaryAgent(p, snap)
	name := agentDisplayName(agent)
	if name == "" {
		return ""
	}
	if agent == agentClaude {
		name = "Claude"
	}
	identity := caller
	if slot := paneSlotsByID(a.ws)[caller]; slot.Number > 0 {
		identity = fmt.Sprintf("#%d", slot.Number)
	}
	// The handle comes first: it is how the receiver should address a reply.
	if h := identityHandle(snap.Identity); h != "" {
		return "@" + h + " · " + identity + " " + name
	}
	return identity + " " + name
}
