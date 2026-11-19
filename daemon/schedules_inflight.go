package main

// RunInFlight names an automation whose run is in flight: a task or a
// built-in agent's run, or a scheduled prompt being typed into its pane. An
// unattended update waits for it (updateBusyCauseAutomation).
func (s *ScheduleStore) RunInFlight() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nameOf := func(id string) string {
		if sched, ok := s.items[id]; ok {
			return noticeNameOf(sched)
		}
		// Deleted while it ran: its process still runs.
		return "a deleted automation"
	}
	for id, running := range s.taskRunning {
		if running {
			return nameOf(id), true
		}
	}
	for id, running := range s.promptRunning {
		if running {
			return nameOf(id), true
		}
	}
	for agentID, running := range s.agentRunning {
		if running {
			return "the " + agentID + " agent", true
		}
	}
	return "", false
}
