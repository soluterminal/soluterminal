package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The ask tools (asks.go) and their HTTP side: GET /api/asks for the app and
// POST /api/asks/{id}/dismiss, which only the person may call.

var askToolNames = map[string]bool{"post_user_ask": true, "resolve_user_ask": true}

// mcpAskToolCall reports whether a tools/call is one of the ask tools.
func mcpAskToolCall(params json.RawMessage) bool {
	var p struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(params, &p) == nil && askToolNames[p.Name]
}

// askCallerTraced reports whether the connection was traced to a process in
// a pane (not a pane named only by SOLU_PANE_ID or by the header).
func askCallerTraced(id callerIdentity) bool {
	return id.Kind == callerPane && id.PaneID != "" && !id.FromEnv
}

func askMCPTools() []mcpTool {
	return []mcpTool{
		{
			Name: "post_user_ask",
			Description: "Put something you need from the user on their \"Waiting on you\" list: a decision " +
				"only they can make or an action only they can take (e.g. \"OK to deploy the site?\", " +
				"\"renew the bot token by 18:00\"). It shows in their bell, sidebar and phone with your " +
				"pane's name, until you resolve it, the condition you set is met, your pane closes, or " +
				"they dismiss it. One ask per decision or action, in one line. Post only your own asks: " +
				"never relay another pane's (that pane posts its own). Posting again with the same key " +
				"updates that ask in place instead of adding one. Once posted, don't restate open asks " +
				"at the end of your replies: the result lists your open asks. Call resolve_user_ask when " +
				"it is done or no longer needed. At most 5 open per pane.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"key": {
						"type": "string",
						"description": "Your short name for this ask, unique among your own asks (1–40 of a-z, 0-9, '.', '_', '-'), e.g. \"deploy-site\". Reuse it to update the ask."
					},
					"text": {
						"type": "string",
						"description": "One line (≤ 240 characters) saying what the user needs to decide or do. Plain text."
					},
					"resolve_on": {
						"type": "string",
						"enum": ["reply"],
						"description": "\"reply\": close the ask on its own once the user next sends a message in your pane (use it for a question they answer you directly). Leave out to keep it open until you resolve it."
					}
				},
				"required": ["key", "text"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "resolve_user_ask",
			Description: "Take one of your own asks off the user's \"Waiting on you\" list, because it is " +
				"done or no longer needed. Returns your remaining open asks.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"key": {
						"type": "string",
						"description": "The key you posted the ask with."
					}
				},
				"required": ["key"],
				"additionalProperties": false
			}`),
		},
	}
}

// askToolCaller is the pane an ask tool acts for, or why there is none. Only
// a pane the daemon traced from the connection's process may post: not an
// unverified claim (SOLU_PANE_ID alone, or a header the daemon could not
// check), not a scoped session or a process the daemon started.
func askToolCaller(caller paneCaller, scope *mcpScope) (string, any) {
	if scope != nil {
		return "", toolErr("asks are for agents in the user's panes; this session can't post them")
	}
	if strings.TrimSpace(caller.Claimed) != "" || strings.TrimSpace(caller.ID) == "" {
		return "", toolErr("SoluTerminal couldn't confirm which pane this call comes from, so it can't post or resolve asks. Ask the user in your reply instead.")
	}
	return caller.ID, nil
}

type askToolOpen struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Age  string `json:"age"`
}

func askAge(ms int64, now time.Time) string {
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (a *apiServer) askOpenSummary(pane string) []askToolOpen {
	now := time.Now()
	out := []askToolOpen{}
	for _, x := range a.asks.openFor(pane) {
		out = append(out, askToolOpen{Key: x.Key, Text: x.Text, Age: askAge(x.CreatedAt, now)})
	}
	return out
}

func (a *apiServer) mcpToolPostUserAsk(args map[string]any, caller paneCaller, scope *mcpScope) any {
	pane, refused := askToolCaller(caller, scope)
	if refused != nil {
		return refused
	}
	if a.asks == nil {
		return toolErr("asks are not available in this daemon")
	}
	p, ok := a.mgr.Get(pane)
	if !ok {
		return toolErr("your pane is not open")
	}
	ask, created, err := a.asks.post(askPost{
		Pane:      pane,
		Source:    a.paneCallerLabel(p),
		Kind:      askKindAgent,
		Key:       mcpArgString(args, "key"),
		Text:      mcpArgString(args, "text"),
		ResolveOn: strings.TrimSpace(mcpArgString(args, "resolve_on")),
	})
	if err != nil {
		return toolErr(err.Error())
	}
	status := "updated"
	if created {
		status = "posted"
	}
	b, _ := json.Marshal(map[string]any{"status": status, "id": ask.ID, "key": ask.Key, "text": ask.Text, "open_asks": a.askOpenSummary(pane)})
	return toolOK(string(b))
}

func (a *apiServer) mcpToolResolveUserAsk(args map[string]any, caller paneCaller, scope *mcpScope) any {
	pane, refused := askToolCaller(caller, scope)
	if refused != nil {
		return refused
	}
	if a.asks == nil {
		return toolErr("asks are not available in this daemon")
	}
	if err := a.asks.resolve(pane, mcpArgString(args, "key")); err != nil {
		if errors.Is(err, errAskNotFound) {
			keys := []string{}
			for _, x := range a.asks.openFor(pane) {
				keys = append(keys, x.Key)
			}
			if len(keys) == 0 {
				return toolErr("you have no open asks")
			}
			return toolErr("no open ask of yours has that key; your open keys: " + strings.Join(keys, ", "))
		}
		return toolErr(err.Error())
	}
	b, _ := json.Marshal(map[string]any{"status": "resolved", "open_asks": a.askOpenSummary(pane)})
	return toolOK(string(b))
}

// askView is an ask as the app shows it. Source is always the daemon's label
// for the posting pane (live when it is open), never text the agent wrote.
type askView struct {
	Ask
	Source string `json:"source"`
	Open   bool   `json:"paneOpen"`
}

func (a *apiServer) listAsks(w http.ResponseWriter, r *http.Request) {
	out := []askView{}
	if a.asks != nil {
		for _, x := range a.asks.list() {
			v := askView{Ask: x, Source: x.Source}
			if p, ok := a.mgr.Get(x.SourcePane); ok {
				v.Source, v.Open = a.paneCallerLabel(p), true
			}
			if v.Source == "" {
				v.Source = "a closed pane"
			}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"asks": out})
}

func (a *apiServer) dismissAsk(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "dismiss an ask") {
		return
	}
	if a.asks == nil {
		http.Error(w, "asks unavailable", http.StatusNotFound)
		return
	}
	switch err := a.asks.dismiss(r.PathValue("id")); {
	case errors.Is(err, errAskNotFound):
		http.Error(w, "ask not found", http.StatusNotFound)
	case errors.Is(err, errAskClosed):
		http.Error(w, "ask is already closed", http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// asksPaneClosed closes the asks of a pane that is gone for good: the person
// or an agent closed it, or it was archived. It is not Manager.Kill's hook,
// which also runs when a pane is respawned in place under the same id (model,
// effort, role, refresh, resume, mode switch); those keep their asks.
func (a *apiServer) asksPaneClosed(id string) {
	if a.asks != nil {
		a.asks.paneGone(id)
	}
}

// publishAsksChanged tells the app to refetch /api/asks. The event carries no
// ask text.
func (a *apiServer) publishAsksChanged() {
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "asks.changed", Ts: time.Now().UnixMilli()})
	}
}

// paneAskDTO is an open ask as /api/panes carries it on its pane. Source is
// the daemon's label for the pane, shown beside the text.
type paneAskDTO struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"createdAt"`
}
