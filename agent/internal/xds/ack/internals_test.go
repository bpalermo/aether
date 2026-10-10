package ack

// What the tests read of the tracker's own state, in one place.

// unanswered is every response the tracker holds in flight, on every stream.
func unanswered(t *Tracker) []inflightResponse {
	t.mu.Lock()
	defer t.mu.Unlock()
	var entries []inflightResponse
	for _, s := range t.streams {
		for _, entry := range s.inflight {
			entries = append(entries, entry)
		}
	}
	return entries
}

// proxies is the number of streams the tracker keeps anything for.
func proxies(t *Tracker) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.streams)
}

// remembered is the number of resource names the tracker keeps anything
// under, over every stream and type: stated, held or rejected.
func remembered(t *Tracker) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, s := range t.streams {
		for _, ts := range s.types {
			names := map[string]struct{}{}
			for name := range ts.stated {
				names[name] = struct{}{}
			}
			for name := range ts.held {
				names[name] = struct{}{}
			}
			for name := range ts.rejected {
				names[name] = struct{}{}
			}
			total += len(names)
		}
	}
	return total
}
