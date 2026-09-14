package engine

import (
	"fmt"
	"sync"
)

// Registry maps engine ids to their Engine implementations. The agentd
// process builds a single Registry at boot (NewDefaultRegistry) and shares it
// across the spawner, control sheet, and ingest pipelines.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Engine
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Engine{}}
}

// Register adds an engine to the registry. Re-registering an existing id is
// a programming error — the lifecycle invariants assume one engine per id.
func (r *Registry) Register(e Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[e.ID()] = e
}

// Get returns the engine for the given id (default Claude when id is empty
// or unknown — every pre-existing row in managed_sessions is a Claude run).
func (r *Registry) Get(id string) (Engine, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[ResolveID(id)]
	if !ok {
		return nil, fmt.Errorf("engine: no engine registered for id %q", id)
	}
	return e, nil
}

// MustGet is Get with a panic on missing — for boot paths where a missing
// engine is unrecoverable.
func (r *Registry) MustGet(id string) Engine {
	e, err := r.Get(id)
	if err != nil {
		panic(err)
	}
	return e
}

// IDs returns the registered engine ids (sorted order is NOT guaranteed).
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	return out
}
