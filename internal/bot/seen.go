package bot

import "sync"

// seenSet remembers recent event IDs so a redelivered event does not start a
// second turn. Slack redelivers an envelope it believes was not acknowledged,
// and a reconnect can overlap deliveries, so this is not optional. M4 moves it
// into SQLite so it survives a restart (DESIGN.md §7).
type seenSet struct {
	mu    sync.Mutex
	max   int
	order []string
	set   map[string]struct{}
}

func newSeenSet(max int) *seenSet {
	return &seenSet{max: max, set: make(map[string]struct{}, max)}
}

// claim reports whether id has not been seen before. An empty id is always
// new: there is nothing to deduplicate on.
func (s *seenSet) claim(id string) bool {
	if id == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	for len(s.order) > s.max {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	return true
}
