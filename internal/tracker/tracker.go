package tracker

import (
	"strings"
	"sync"
	"time"
)

// Candidate is one scheduler auth candidate reduced to what the tracker needs.
type Candidate struct {
	// AuthID identifies the auth record. It is matchable as auth_id.
	AuthID string
	// Provider identifies the auth provider. It is matchable as provider.
	Provider string
	// Attributes are the candidate's immutable routing attributes.
	Attributes map[string]string
}

// Decision is the outcome of a scheduler pick.
type Decision struct {
	// Handled reports whether the plugin made a scheduling decision.
	Handled bool
	// AuthID is the selected auth record when Handled is true.
	AuthID string
	// Rejected reports that every backend in the matching pool is full and that
	// pool's policy is reject. Handled is false in that case; the caller answers
	// with an HTTP 429 scheduler error.
	Rejected bool
	// Backend is the display name of the chosen backend when Handled is true.
	Backend string
	// Pool is the display name of the pool that served the pick.
	Pool string
}

// Tracker accounts for in-flight and reserved slots per backend. Slot state is
// keyed by backend match signature, so a backend that appears in several pools
// shares one counter: it is one physical machine.
//
// Every exported method is safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	now   func() time.Time
	cfg   Config
	order []*backendState
	bySig map[string]*backendState
	pools []*poolState
	// aliasPool maps a lower-cased group alias to the pool that owns it.
	// Configuration guarantees an alias belongs to at most one pool.
	aliasPool map[string]*poolState
	// authBackend maps an auth ID to a backend signature. It is learned from
	// scheduler candidates, including picks the plugin does not handle, so that
	// direct-alias traffic can be attributed to the right backend.
	authBackend map[string]string
	// requestBackend maps a live request ID to the backend signature holding
	// its in-flight slot.
	requestBackend map[string]string
}

type poolState struct {
	pool     Pool
	backends []*backendState
}

type backendState struct {
	backend  Backend
	sig      string
	inflight int
	// pending holds the expiry instant of each unclaimed pick reservation.
	pending []time.Time
}

// New builds a tracker for cfg. A nil now falls back to time.Now, which lets
// tests drive the pending-reservation clock without sleeping.
func New(cfg Config, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{
		now:            now,
		authBackend:    make(map[string]string),
		requestBackend: make(map[string]string),
	}
	t.applyConfig(cfg)
	return t
}

// Reconfigure swaps in a new configuration while preserving the live in-flight
// and pending counts of every backend whose match set is unchanged. Backends
// that disappear lose their counts, and bindings that pointed at them are
// dropped so a later completion is a harmless no-op.
func (t *Tracker) Reconfigure(cfg Config) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.applyConfig(cfg)
}

func (t *Tracker) applyConfig(cfg Config) {
	previous := t.bySig
	t.cfg = cfg
	t.order = make([]*backendState, 0, len(cfg.Backends))
	t.bySig = make(map[string]*backendState, len(cfg.Backends))
	for _, backend := range cfg.Backends {
		sig := backend.Signature()
		state := &backendState{backend: backend, sig: sig}
		if carried, ok := previous[sig]; ok {
			state.inflight = carried.inflight
			state.pending = carried.pending
		}
		t.order = append(t.order, state)
		t.bySig[sig] = state
	}

	t.pools = make([]*poolState, 0, len(cfg.Pools))
	t.aliasPool = make(map[string]*poolState)
	for _, pool := range cfg.Pools {
		state := &poolState{pool: pool, backends: make([]*backendState, 0, len(pool.Backends))}
		for _, backend := range pool.Backends {
			if backendState, ok := t.bySig[backend.Signature()]; ok {
				state.backends = append(state.backends, backendState)
			}
		}
		t.pools = append(t.pools, state)
		for _, alias := range pool.GroupAliases {
			t.aliasPool[alias] = state
		}
	}

	for authID, sig := range t.authBackend {
		if _, ok := t.bySig[sig]; !ok {
			delete(t.authBackend, authID)
		}
	}
	for requestID, sig := range t.requestBackend {
		if _, ok := t.bySig[sig]; !ok {
			delete(t.requestBackend, requestID)
		}
	}
}

// Config returns the configuration currently in effect.
func (t *Tracker) Config() Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// Pick chooses a backend for a scheduler request. It always records the auth
// ID to backend mapping for every candidate it can resolve, even when it leaves
// the pick unhandled, so that later direct-alias traffic is still counted.
func (t *Tracker) Pick(model, requestedModel string, candidates []Candidate) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	// firstAuth keeps the first candidate offered for each backend, in the
	// order the host listed them.
	firstAuth := make(map[string]string, len(t.order))
	for _, candidate := range candidates {
		state := t.matchLocked(candidate)
		if state == nil {
			continue
		}
		if authID := strings.TrimSpace(candidate.AuthID); authID != "" {
			t.authBackend[authID] = state.sig
			if _, seen := firstAuth[state.sig]; !seen {
				firstAuth[state.sig] = authID
			}
		}
	}

	pool := t.poolForLocked(model, requestedModel)
	if pool == nil {
		return Decision{}
	}

	available := make([]*backendState, 0, len(pool.backends))
	for _, state := range pool.backends {
		if _, ok := firstAuth[state.sig]; ok {
			available = append(available, state)
		}
	}
	if len(available) == 0 {
		return Decision{}
	}

	for _, state := range available {
		if state.load() < state.backend.MaxConcurrency {
			return t.reserveLocked(pool, state, firstAuth[state.sig])
		}
	}

	switch pool.pool.OnSaturated {
	case PolicyReject:
		return Decision{Rejected: true, Pool: pool.pool.Name}
	case PolicyFirst:
		return t.reserveLocked(pool, available[0], firstAuth[available[0].sig])
	default:
		chosen := available[0]
		best := chosen.ratio()
		for _, state := range available[1:] {
			if ratio := state.ratio(); ratio < best {
				chosen = state
				best = ratio
			}
		}
		return t.reserveLocked(pool, chosen, firstAuth[chosen.sig])
	}
}

// Bind attaches a request to the backend that owns the selected auth ID.
//
// Only a request whose model targets a group alias consumes a pick
// reservation, because only a steered pick creates one. A direct-alias request
// still takes an in-flight slot, but consuming a reservation on its behalf
// would free a slot belonging to a concurrent group request and let that
// backend be oversubscribed by one.
//
// Rebinding a request that was already counted against another backend, which
// happens when the host retries onto a different credential, releases the old
// slot first and then claims a reservation on the new backend when the request
// is steered. Binding to a backend the plugin does not manage only releases.
//
// model and requestedModel come from the request.intercept_after payload and
// are tested with the same steering rule scheduler picks use.
func (t *Tracker) Bind(requestID, authID, model, requestedModel string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	sig, managed := t.authBackend[strings.TrimSpace(authID)]
	if current, bound := t.requestBackend[requestID]; bound {
		if managed && current == sig {
			return
		}
		t.releaseLocked(current)
		delete(t.requestBackend, requestID)
	}
	if !managed {
		return
	}
	state, ok := t.bySig[sig]
	if !ok {
		return
	}
	state.inflight++
	if t.poolForLocked(model, requestedModel) != nil {
		state.consumePending()
	}
	t.requestBackend[requestID] = sig
}

// Complete releases the slot held by a request. It is idempotent and ignores
// request IDs it never bound.
func (t *Tracker) Complete(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	sig, bound := t.requestBackend[requestID]
	if !bound {
		return
	}
	delete(t.requestBackend, requestID)
	t.releaseLocked(sig)
}

func (t *Tracker) reserveLocked(pool *poolState, state *backendState, authID string) Decision {
	state.pending = append(state.pending, t.now().Add(t.cfg.PendingTTL))
	return Decision{Handled: true, AuthID: authID, Backend: state.backend.Name, Pool: pool.pool.Name}
}

func (t *Tracker) releaseLocked(sig string) {
	state, ok := t.bySig[sig]
	if !ok {
		return
	}
	if state.inflight > 0 {
		state.inflight--
	}
}

// poolForLocked returns the pool steering this request, or nil when neither
// model is a configured group alias. Both the execution model and the
// client-requested model are checked because the host may already have
// rewritten the alias by the time the scheduler runs.
func (t *Tracker) poolForLocked(model, requestedModel string) *poolState {
	if pool, ok := t.aliasPool[strings.ToLower(strings.TrimSpace(model))]; ok {
		return pool
	}
	if pool, ok := t.aliasPool[strings.ToLower(strings.TrimSpace(requestedModel))]; ok {
		return pool
	}
	return nil
}

// matchLocked returns the first configured backend whose match set is fully
// satisfied by the candidate. Keys and values compare case-insensitively. The
// reserved keys provider and auth_id read the candidate's own fields and
// shadow any attribute of the same name.
func (t *Tracker) matchLocked(candidate Candidate) *backendState {
	normalized := make(map[string]string, len(candidate.Attributes)+2)
	for key, value := range candidate.Attributes {
		normalized[strings.ToLower(strings.TrimSpace(key))] = strings.ToLower(strings.TrimSpace(value))
	}
	if provider := strings.ToLower(strings.TrimSpace(candidate.Provider)); provider != "" {
		normalized[MatchKeyProvider] = provider
	} else {
		delete(normalized, MatchKeyProvider)
	}
	if authID := strings.ToLower(strings.TrimSpace(candidate.AuthID)); authID != "" {
		normalized[MatchKeyAuthID] = authID
	} else {
		delete(normalized, MatchKeyAuthID)
	}
	if len(normalized) == 0 {
		return nil
	}

	for _, state := range t.order {
		matched := true
		for key, want := range state.backend.Match {
			if got, ok := normalized[key]; !ok || got != want {
				matched = false
				break
			}
		}
		if matched {
			return state
		}
	}
	return nil
}

// pruneLocked drops pick reservations that were never claimed within the TTL.
func (t *Tracker) pruneLocked() {
	now := t.now()
	for _, state := range t.order {
		if len(state.pending) == 0 {
			continue
		}
		kept := state.pending[:0]
		for _, expiry := range state.pending {
			if expiry.After(now) {
				kept = append(kept, expiry)
			}
		}
		state.pending = kept
	}
}

func (s *backendState) load() int {
	return s.inflight + len(s.pending)
}

func (s *backendState) ratio() float64 {
	return float64(s.load()) / float64(s.backend.MaxConcurrency)
}

// consumePending drops the reservation closest to expiry, which is the one the
// pick that led to this bind most likely created.
func (s *backendState) consumePending() {
	if len(s.pending) == 0 {
		return
	}
	earliest := 0
	for index, expiry := range s.pending[1:] {
		if expiry.Before(s.pending[earliest]) {
			earliest = index + 1
		}
	}
	s.pending = append(s.pending[:earliest], s.pending[earliest+1:]...)
}
