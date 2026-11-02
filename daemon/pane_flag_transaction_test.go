package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type failingFirstFlagStore struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type blockingFlagStore struct {
	*PaneStore
	entered chan struct{}
	release chan struct{}
	removed chan struct{}
	calls   atomic.Int32
}

func (s *blockingFlagStore) Put(p *Pane) error {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		<-s.release
	}
	return s.PaneStore.Put(p)
}

func (s *blockingFlagStore) Remove(id string) error {
	close(s.removed)
	return s.PaneStore.Remove(id)
}

func TestFlagPersistenceSerializesReplacementAndRollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", rollback), func(t *testing.T) {
			mgr := NewManager(t.TempDir())
			old := testPaneWithTail(t, "pane-transaction", "old", "$ ")
			mgr.panes[old.ID] = old
			persist, err := NewPaneStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store := &blockingFlagStore{PaneStore: persist, entered: make(chan struct{}), release: make(chan struct{}), removed: make(chan struct{})}
			defer func() {
				select {
				case <-store.release:
				default:
					close(store.release)
				}
			}()
			flagDone := make(chan error, 1)
			go func() { flagDone <- mgr.changeCurrentFlag(old, store, nil, true, "old reminder", "user") }()
			select {
			case <-store.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("flag did not enter persistence")
			}
			started, finished := make(chan struct{}), make(chan struct{})
			replacement := testPaneWithTail(t, old.ID, "replacement", "$ ")
			go func() {
				defer close(finished)
				if rollback {
					a := &apiServer{mgr: mgr, panes: store, killPane: func(id string) error {
						close(started)
						mgr.mu.Lock()
						delete(mgr.panes, id)
						mgr.mu.Unlock()
						return nil
					}}
					a.rollbackSpawnedPane(old)
				} else {
					close(started)
					mgr.mu.Lock()
					mgr.panes[old.ID] = replacement
					mgr.mu.Unlock()
					if err := store.Put(replacement); err != nil {
						t.Error(err)
					}
				}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle change did not start")
			}
			select {
			case <-finished:
				t.Fatal("lifecycle change crossed in-flight persistence")
			case <-store.removed:
				t.Fatal("rollback removed persistence before fencing old writes")
			case <-time.After(10 * time.Millisecond):
			}
			close(store.release)
			select {
			case err := <-flagDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("flag transaction stuck")
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle change stuck")
			}
			saved := store.List()
			if rollback {
				if len(saved) != 0 {
					t.Fatalf("rollback resurrected pane: %+v", saved)
				}
			} else if len(saved) != 1 || saved[0].Title != "replacement" || saved[0].Flagged {
				t.Fatalf("old flag overwrote replacement: %+v", saved)
			}
		})
	}
}

func (s *failingFirstFlagStore) List() []PersistedPane { return nil }
func (s *failingFirstFlagStore) Remove(string) error   { return nil }
func (s *failingFirstFlagStore) Put(*Pane) error {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		<-s.release
		return errors.New("disk full")
	}
	return nil
}

func TestFailedFlagSaveCannotRollbackLaterManualFlag(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "pane-rollback", "worker", "$ ")
	p.SetFlagAs(true, "old reminder", "agent:caller")
	mgr := testManagerWithPane(p)
	store := &failingFirstFlagStore{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	first := make(chan error, 1)
	go func() { first <- mgr.changeCurrentFlag(p, store, nil, false, "", "agent:caller") }()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not reach persistence")
	}
	manual := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		manual <- mgr.changeCurrentFlag(p, store, nil, true, "new manual reminder", "user")
	}()
	<-started
	select {
	case <-manual:
		t.Fatal("manual write crossed in-flight flag persistence")
	case <-time.After(10 * time.Millisecond):
	}
	close(store.release)
	select {
	case err := <-first:
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatal("expected persistence failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first write stuck")
	}
	select {
	case err := <-manual:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manual write stuck")
	}
	if got := p.Snapshot(); !got.Flagged || got.FlagMemo != "new manual reminder" || got.FlagOwner != "user" {
		t.Fatalf("failed save rolled back manual replacement: %+v", got)
	}
}
