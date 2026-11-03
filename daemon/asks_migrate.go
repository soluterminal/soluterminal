package main

import (
	"errors"
	"log"
	"strings"
)

// setPaneFlagAgentNotice answers an agent in a pane that calls set_pane_flag.
const setPaneFlagAgentNotice = "Flags are the user's own bookmarks; agents no longer set or clear them. " +
	"To ask the user to decide or do something, call post_user_ask (one ask per item, your own only); " +
	"resolve it with resolve_user_ask when it is done."

// migrateFlagsToAsks moves the agent-to-person flags of earlier versions onto
// the asks list at boot, so flags are left as the person's bookmarks:
//   - the worker manager's notices become worker asks on their lead
//     (migrateWorkerManagerFlags);
//   - an agent's flag ("agent", or "agent:<caller pane>") becomes an ask from
//     that caller when its pane is still open, else from the flagged pane;
//   - a Warden flag is cleared.
//
// The person's flags ("user", and older ones with no owner) are untouched.
// Once no such flag is left it does nothing.
func (a *apiServer) migrateFlagsToAsks() {
	a.migrateWorkerManagerFlags()
	for _, p := range a.mgr.List() {
		snap := p.Snapshot()
		if !snap.Flagged {
			continue
		}
		owner := snap.FlagOwner
		switch {
		case owner == "agent" || strings.HasPrefix(owner, "agent:"):
			if a.asks == nil {
				continue
			}
			source := p
			if id, ok := strings.CutPrefix(owner, "agent:"); ok {
				if caller, open := a.mgr.Get(id); open {
					source = caller
				}
			}
			text := askText(snap.FlagMemo)
			if text == "" {
				text = "Come back to this pane"
			}
			if source != p {
				text = "On " + a.paneCallerLabel(p) + ": " + text
			}
			if _, _, err := a.asks.post(askPost{Pane: source.ID, Source: a.paneCallerLabel(source), Kind: askKindAgent, Key: "flag-" + strings.ToLower(p.ID), Text: text, Internal: true}); err != nil {
				log.Printf("asks: migrate the flag on %s: %v", p.ID, err)
				continue
			}
		case owner == "warden":
		default:
			continue
		}
		if err := a.mgr.changeCurrentFlag(p, a.panes, func(prev PaneSnapshot) error {
			if !prev.Flagged || prev.FlagOwner != owner {
				return errors.New("flag changed")
			}
			return nil
		}, false, "", ""); err != nil {
			log.Printf("asks: clear the migrated flag on %s: %v", p.ID, err)
		}
	}
}
