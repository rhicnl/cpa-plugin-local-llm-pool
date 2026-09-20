package tracker

// Status is the payload served by the plugin's /status management resource.
type Status struct {
	// Pools describes each configured pool and its fill order.
	Pools []PoolStatus `json:"pools"`
	// Backends holds one entry per distinct physical backend. A backend shared
	// by several pools appears once, with the counter all of them share.
	Backends []BackendStatus `json:"backends"`
	Totals   Totals          `json:"totals"`
	Config   ConfigStatus    `json:"config"`
}

// PoolStatus reports one pool's aliases, policy and fill order.
type PoolStatus struct {
	Name         string   `json:"name"`
	GroupAliases []string `json:"group_aliases"`
	OnSaturated  string   `json:"on_saturated"`
	// Backends lists the names of this pool's backends in fill order. The
	// counts live in the top-level backends list.
	Backends []string `json:"backends"`
}

// BackendStatus reports the live slot accounting of one physical backend.
type BackendStatus struct {
	Name           string            `json:"name"`
	Match          map[string]string `json:"match"`
	MaxConcurrency int               `json:"max_concurrency"`
	Inflight       int               `json:"inflight"`
	Pending        int               `json:"pending"`
	// Available is max_concurrency minus inflight and pending, floored at zero.
	Available int `json:"available"`
}

// Totals aggregates every backend.
type Totals struct {
	MaxConcurrency int `json:"max_concurrency"`
	Inflight       int `json:"inflight"`
	Pending        int `json:"pending"`
	Available      int `json:"available"`
	// TrackedRequests counts request IDs currently holding a slot.
	TrackedRequests int `json:"tracked_requests"`
}

// ConfigStatus echoes the configuration in effect. GroupAliases and
// OnSaturated keep their original single-pool meaning for consumers written
// before pools existed: the aliases are the union across pools, and the policy
// is the top-level default that pools inherit.
type ConfigStatus struct {
	GroupAliases []string `json:"group_aliases"`
	OnSaturated  string   `json:"on_saturated"`
	PendingTTLMS int64    `json:"pending_ttl_ms"`
}

// Status snapshots the tracker for the management resource. Expired pick
// reservations are pruned first so the report never shows stale pending slots.
func (t *Tracker) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	aliases := t.cfg.GroupAliases()
	if aliases == nil {
		aliases = []string{}
	}
	out := Status{
		Pools:    make([]PoolStatus, 0, len(t.pools)),
		Backends: make([]BackendStatus, 0, len(t.order)),
		Config: ConfigStatus{
			GroupAliases: aliases,
			OnSaturated:  string(t.cfg.OnSaturated),
			PendingTTLMS: t.cfg.PendingTTL.Milliseconds(),
		},
	}

	for _, pool := range t.pools {
		entry := PoolStatus{
			Name:         pool.pool.Name,
			GroupAliases: append([]string(nil), pool.pool.GroupAliases...),
			OnSaturated:  string(pool.pool.OnSaturated),
			Backends:     make([]string, 0, len(pool.backends)),
		}
		for _, state := range pool.backends {
			entry.Backends = append(entry.Backends, state.backend.Name)
		}
		out.Pools = append(out.Pools, entry)
	}

	for _, state := range t.order {
		match := make(map[string]string, len(state.backend.Match))
		for key, value := range state.backend.Match {
			match[key] = value
		}
		entry := BackendStatus{
			Name:           state.backend.Name,
			Match:          match,
			MaxConcurrency: state.backend.MaxConcurrency,
			Inflight:       state.inflight,
			Pending:        len(state.pending),
			Available:      max(state.backend.MaxConcurrency-state.load(), 0),
		}
		out.Backends = append(out.Backends, entry)
		out.Totals.MaxConcurrency += entry.MaxConcurrency
		out.Totals.Inflight += entry.Inflight
		out.Totals.Pending += entry.Pending
		out.Totals.Available += entry.Available
	}
	out.Totals.TrackedRequests = len(t.requestBackend)
	return out
}
