package main

import "fmt"

// wardenPrompt builds the judgment prompt for a summoned run. The run has
// ONLY the read+annotate MCP tools (wardenReadAnnotateTools) — the prompt is
// written to that budget and never asks for capability it doesn't have.
//
// Hard rules shared by both event kinds:
//   - Never quote a credential/token/secret into your output. If a tail shows
//     one, refer to it obliquely.
//   - Only an opted-in triage may set an episode-bound flag. The daemon
//     enforces the current option and episode independently of prompt text.
//   - Propose lessons; never claim to have applied them. You cannot edit files.
//   - Be terse. The user reads many of these.

const wardenPromptRules = `You are the soluterminal Warden — a background caretaker for a fleet of terminal/agent panes. You have a RESTRICTED tool set: you can read pane state/tails, the activity feed, ports and git status. You cannot type into panes, spawn/kill panes, run commands, or edit files.

Rules:
- Never quote a secret, token, API key, or password in your output — refer to it obliquely if relevant.
- List concrete follow-ups in your reply; it is saved in Warden history and the inbox. Propose lessons as text; never claim to have applied them.
- Be terse and specific. Lead with what the user must decide or do.`

func wardenPrompt(ev wardenEvent) string {
	switch ev.Kind {
	case wardenTriage:
		flagInstruction := "Do not set a pane flag. The automatic flag option is off; your brief is still saved in Warden history and the inbox."
		if ev.AutoFlags {
			flagInstruction = fmt.Sprintf("Set one flag on pane %s if it is unflagged, with a memo stating what it needs, key context, and your recommended answer. Do not clear or overwrite a flag.", ev.PaneID)
		}
		return fmt.Sprintf(`%s

TASK: TRIAGE. Pane %s ("%s") has been waiting for the user (%s) longer than expected — the real-time alert already fired and they haven't come. Read the pane's recent tail and state, understand exactly what it is blocked on, and:
1. %s
2. In your reply, give a one-paragraph brief the user can read at a glance.
Do NOT answer the prompt yourself — you cannot, and the decision is the user's.`,
			wardenPromptRules, ev.PaneID, ev.PaneTitle, safeReason(ev.Reason), flagInstruction)

	case wardenDebrief:
		return fmt.Sprintf(`%s

TASK: DEBRIEF. Pane %s ("%s") just finished (%s). Read its tail to reconstruct what the session did, then report:
1. OUTCOME — one or two lines on what was accomplished or left unfinished.
2. FOLLOW-UPS — list each concrete unfinished thread, one line each.
3. LESSONS — propose any hard-won, reusable lesson (a gotcha, a non-obvious constraint) as text for the user to promote later. If there are none, say so.
Keep it tight — no flag needed for a clean finish.`,
			wardenPromptRules, ev.PaneID, ev.PaneTitle, safeReason(ev.Reason))

	case wardenBrief:
		return wardenPromptRules + `

TASK: FLEET BRIEF. Give the user a "where was I?" rollup of the whole fleet. Call list_panes, then get_pane_state and (as needed) read_pane_tail for each. Organize your reply as:
- NEEDS A DECISION — panes waiting on the user, each with what it needs and your recommendation.
- FINISHED / IDLE — panes that completed or went quiet, one line each.
- STILL WORKING — panes actively busy, one line each (no action).
- HYGIENE — dirty shared trees, stale panes, port conflicts worth noting (use list_ports / git_status).
Lead with what needs the user. Be terse. Do not set flags in a fleet brief.`
	}
	return wardenPromptRules
}

func safeReason(r string) string {
	if r == "" {
		return "no reason recorded"
	}
	return r
}
