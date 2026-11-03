package main

import (
	"sort"
	"strings"
	"testing"
)

// leadNoticed reports whether the worker manager has a notice open on the
// lead: one or more worker asks (worker_archives.go).
func leadNoticed(a *apiServer, lead string) bool { return len(a.asks.workerAsks(lead)) > 0 }

// leadNoticeText is the lead's open worker asks, one per line, sorted.
func leadNoticeText(a *apiServer, lead string) string {
	var texts []string
	for _, x := range a.asks.workerAsks(lead) {
		texts = append(texts, x.Text)
	}
	sort.Strings(texts)
	return strings.Join(texts, "\n")
}

// dismissLeadNotices is the person dismissing every worker ask on the lead.
func dismissLeadNotices(t *testing.T, a *apiServer, lead string) {
	t.Helper()
	for _, x := range a.asks.workerAsks(lead) {
		if err := a.asks.dismiss(x.ID); err != nil {
			t.Fatal(err)
		}
	}
}
