package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// CreateEmpty creates a visible workspace without replacing any existing view.
// Existence and persistence share the store lock, including case-folded names.
func (s *WorkspaceStore) CreateEmpty(name string) (*Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "_current") {
		return nil, fmt.Errorf("choose a visible named workspace")
	}
	if utf8.RuneCountInString(name) > 120 {
		return nil, fmt.Errorf("workspace name must have at most 120 characters")
	}
	if err := validateWorkspaceConfirmName(name); err != nil {
		return nil, fmt.Errorf("workspace name must not contain control or bidi characters")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for existing := range s.data {
		if strings.EqualFold(existing, name) {
			return nil, fmt.Errorf("workspace %q already exists; call list_workspaces", existing)
		}
	}
	w := &Workspace{Name: name, PaneIds: []string{}, ActiveAt: time.Now().UTC()}
	candidate := s.candidate()
	candidate[name] = w
	if err := s.commit(candidate); err != nil {
		return nil, err
	}
	return cloneWorkspace(w), nil
}
