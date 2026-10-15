package policy

import "sync/atomic"

// snapshot is an immutable set of policies by name. Once published it is only
// ever read: a reload builds a new map and swaps it in, never edits this one.
type snapshot map[string]Policy

// Store holds the current policies. Get takes no lock, so every request can
// read while a reload runs; Reload swaps one pointer.
type Store struct {
	current atomic.Pointer[snapshot]
}

// NewStore returns an empty Store: Get finds nothing until the first Reload.
func NewStore() *Store {
	return &Store{}
}

// Get returns the policy called name. It returns a copy, so a caller cannot
// change what the store holds.
func (s *Store) Get(name string) (Policy, bool) {
	snap := s.current.Load()
	if snap == nil {
		return Policy{}, false
	}
	p, ok := (*snap)[name]
	return p, ok
}

// Reload reads and validates the file at path and, only if that succeeds, swaps
// it in. On any error the previous policies stay in force.
func (s *Store) Reload(path string) error {
	ps, err := LoadFile(path)
	if err != nil {
		return err
	}
	next := make(snapshot, len(ps))
	for _, p := range ps {
		next[p.Name] = p
	}
	s.current.Store(&next)
	return nil
}
