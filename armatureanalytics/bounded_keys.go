package armatureanalytics

import "sync"

type boundedKeySet struct {
	mu    sync.Mutex
	max   int
	keys  map[any]struct{}
	order []any
}

func newBoundedKeySet(maxEntries int) *boundedKeySet {
	return &boundedKeySet{max: maxEntries, keys: make(map[any]struct{})}
}

// Add returns true only when key was newly added. Oldest keys are evicted so
// high session churn cannot grow the recorder forever; stable event IDs make
// a later re-emission harmless at ingest.
func (s *boundedKeySet) Add(key any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[key]; exists {
		return false
	}
	if len(s.order) >= s.max {
		delete(s.keys, s.order[0])
		s.order = s.order[1:]
	}
	s.keys[key] = struct{}{}
	s.order = append(s.order, key)
	return true
}

// Delete forgets a key when the framework provides a reliable session-close
// signal. Missing keys are harmless.
func (s *boundedKeySet) Delete(key any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[key]; !exists {
		return
	}
	delete(s.keys, key)
	for i, existing := range s.order {
		if existing == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
