// Package tracker holds the plugin's slot accounting and configuration logic.
// It is pure Go so it can be tested without cgo and without a running host.
package tracker

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SaturationPolicy decides which backend receives a pick once every backend in
// a pool is at or above its configured concurrency limit.
type SaturationPolicy string

const (
	// PolicyLeastLoaded picks the backend with the lowest load ratio.
	PolicyLeastLoaded SaturationPolicy = "least-loaded"
	// PolicyFirst picks the first backend in configuration order.
	PolicyFirst SaturationPolicy = "first"
	// PolicyReject fails the pick so the host surfaces an HTTP 429.
	PolicyReject SaturationPolicy = "reject"
)

// DefaultPendingTTL is used when pending_ttl_ms is omitted.
const DefaultPendingTTL = 5 * time.Second

// Reserved match keys are compared against fields of the scheduler candidate
// rather than against its Attributes map. An attribute that happens to carry
// one of these names is shadowed by the candidate field.
const (
	// MatchKeyProvider matches SchedulerAuthCandidate.Provider.
	MatchKeyProvider = "provider"
	// MatchKeyAuthID matches SchedulerAuthCandidate.ID.
	MatchKeyAuthID = "auth_id"
)

// Config is a validated plugin configuration.
type Config struct {
	// OnSaturated is the default policy pools inherit when they set none.
	OnSaturated SaturationPolicy
	// PendingTTL bounds how long a pick reservation survives without a bind.
	PendingTTL time.Duration
	// Pools holds the configured pools in configuration order. The shorthand
	// top-level form produces exactly one pool.
	Pools []Pool
	// Backends lists every distinct physical backend once, in the order it was
	// first seen. A backend used by two pools appears here once and shares one
	// slot counter.
	Backends []Backend
}

// Pool is one set of group aliases served by an ordered list of backends.
type Pool struct {
	// Name is a display name derived from the pool's first group alias.
	Name string
	// GroupAliases are the lower-cased model aliases this pool steers.
	GroupAliases []string
	// OnSaturated is the policy for this pool, already resolved against the
	// top-level default.
	OnSaturated SaturationPolicy
	// Backends is the fill order for this pool.
	Backends []Backend
}

// Backend is one validated backend entry.
type Backend struct {
	// Name is a display name derived from Match. It is unique within a Config.
	Name string
	// Match holds the constraints, with keys and values lower-cased. A
	// candidate matches only when it satisfies every key. Keys other than the
	// reserved MatchKeyProvider and MatchKeyAuthID are read from the
	// candidate's Attributes.
	Match map[string]string
	// MaxConcurrency is the number of simultaneous requests the backend accepts.
	MaxConcurrency int
}

// Signature is a stable identity for a backend derived from its match set.
// It survives a reconfigure that only changes max_concurrency, which is how
// live in-flight counts are carried across reloads, and it is what makes a
// backend listed in two pools share one counter.
func (b Backend) Signature() string {
	return joinMatch(b.Match, "\x00")
}

// GroupAliases returns every steered alias across all pools, in pool order.
func (c Config) GroupAliases() []string {
	out := make([]string, 0, len(c.Pools))
	for _, pool := range c.Pools {
		out = append(out, pool.GroupAliases...)
	}
	return out
}

// rawConfig mirrors the YAML the host hands to plugin.register. The host always
// injects enabled and priority, and may inject store, so unknown keys are
// tolerated rather than rejected.
type rawConfig struct {
	GroupAliases []string     `yaml:"group_aliases"`
	OnSaturated  string       `yaml:"on_saturated"`
	PendingTTLMS *int         `yaml:"pending_ttl_ms"`
	Backends     []rawBackend `yaml:"backends"`
	Pools        []rawPool    `yaml:"pools"`
}

type rawPool struct {
	GroupAliases []string     `yaml:"group_aliases"`
	OnSaturated  string       `yaml:"on_saturated"`
	Backends     []rawBackend `yaml:"backends"`
}

type rawBackend struct {
	Match          map[string]string `yaml:"match"`
	MaxConcurrency int               `yaml:"max_concurrency"`
}

// ParseConfig decodes and validates the plugin configuration YAML. It accepts
// two mutually exclusive shapes: the shorthand top-level group_aliases plus
// backends, which describes exactly one pool, and an explicit pools list.
func ParseConfig(raw []byte) (Config, error) {
	var parsed rawConfig
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			return Config{}, fmt.Errorf("decode plugin config: %w", err)
		}
	}

	cfg := Config{}

	defaultPolicy, err := parsePolicy(parsed.OnSaturated, PolicyLeastLoaded, "on_saturated")
	if err != nil {
		return Config{}, err
	}
	cfg.OnSaturated = defaultPolicy

	cfg.PendingTTL = DefaultPendingTTL
	if parsed.PendingTTLMS != nil {
		if *parsed.PendingTTLMS <= 0 {
			return Config{}, fmt.Errorf("pending_ttl_ms must be greater than zero: got %d", *parsed.PendingTTLMS)
		}
		cfg.PendingTTL = time.Duration(*parsed.PendingTTLMS) * time.Millisecond
	}

	shorthand := len(parsed.GroupAliases) > 0 || len(parsed.Backends) > 0
	if shorthand && len(parsed.Pools) > 0 {
		return Config{}, fmt.Errorf("configure either the top-level group_aliases and backends form or pools, not both")
	}
	rawPools := parsed.Pools
	if shorthand {
		rawPools = []rawPool{{GroupAliases: parsed.GroupAliases, Backends: parsed.Backends}}
	}
	if len(rawPools) == 0 {
		return Config{}, fmt.Errorf("configure at least one pool, either with top-level group_aliases and backends or with a pools list")
	}

	registry := newBackendRegistry()
	aliasOwner := make(map[string]int)
	cfg.Pools = make([]Pool, 0, len(rawPools))
	for index, entry := range rawPools {
		// Shorthand errors keep the flat field names so existing configurations
		// report the same way they always have.
		prefix := ""
		if !shorthand {
			prefix = fmt.Sprintf("pools[%d].", index)
		}

		pool := Pool{}
		pool.GroupAliases, err = parseAliases(entry.GroupAliases, prefix)
		if err != nil {
			return Config{}, err
		}
		for _, alias := range pool.GroupAliases {
			if owner, taken := aliasOwner[alias]; taken {
				return Config{}, fmt.Errorf("%sgroup_aliases lists %q, which pools[%d] already claims: an alias may belong to only one pool", prefix, alias, owner)
			}
			aliasOwner[alias] = index
		}
		pool.Name = pool.GroupAliases[0]

		pool.OnSaturated, err = parsePolicy(entry.OnSaturated, defaultPolicy, prefix+"on_saturated")
		if err != nil {
			return Config{}, err
		}

		pool.Backends, err = registry.addPoolBackends(entry.Backends, prefix)
		if err != nil {
			return Config{}, err
		}
		cfg.Pools = append(cfg.Pools, pool)
	}

	cfg.Backends = registry.backends()
	assignBackendNames(cfg.Backends)
	// Pool entries hold copies, so propagate the names the registry just fixed.
	byName := make(map[string]string, len(cfg.Backends))
	for _, backend := range cfg.Backends {
		byName[backend.Signature()] = backend.Name
	}
	for poolIndex := range cfg.Pools {
		for backendIndex := range cfg.Pools[poolIndex].Backends {
			backend := &cfg.Pools[poolIndex].Backends[backendIndex]
			backend.Name = byName[backend.Signature()]
		}
	}

	return cfg, nil
}

func parsePolicy(value string, fallback SaturationPolicy, field string) (SaturationPolicy, error) {
	switch policy := SaturationPolicy(strings.ToLower(strings.TrimSpace(value))); policy {
	case "":
		return fallback, nil
	case PolicyLeastLoaded, PolicyFirst, PolicyReject:
		return policy, nil
	default:
		return "", fmt.Errorf("%s must be one of least-loaded, first, reject: got %q", field, value)
	}
}

func parseAliases(values []string, prefix string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, alias := range values {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, alias)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%sgroup_aliases must list at least one model alias", prefix)
	}
	return out, nil
}

// backendRegistry deduplicates backends across pools by match signature so one
// physical backend is tracked once no matter how many pools reference it.
type backendRegistry struct {
	order []Backend
	bySig map[string]int
}

func newBackendRegistry() *backendRegistry {
	return &backendRegistry{bySig: make(map[string]int)}
}

func (r *backendRegistry) backends() []Backend {
	return r.order
}

func (r *backendRegistry) addPoolBackends(entries []rawBackend, prefix string) ([]Backend, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%sbackends must list at least one backend", prefix)
	}
	out := make([]Backend, 0, len(entries))
	inPool := make(map[string]int, len(entries))
	for index, entry := range entries {
		backend, err := parseBackend(entry, prefix, index)
		if err != nil {
			return nil, err
		}
		signature := backend.Signature()
		if previous, dup := inPool[signature]; dup {
			return nil, fmt.Errorf("%sbackends[%d].match duplicates %sbackends[%d].match", prefix, index, prefix, previous)
		}
		inPool[signature] = index

		if existing, known := r.bySig[signature]; known {
			if r.order[existing].MaxConcurrency != backend.MaxConcurrency {
				return nil, fmt.Errorf("%sbackends[%d] sets max_concurrency %d for a backend already configured with %d: one backend must have one limit",
					prefix, index, backend.MaxConcurrency, r.order[existing].MaxConcurrency)
			}
			out = append(out, r.order[existing])
			continue
		}
		r.bySig[signature] = len(r.order)
		r.order = append(r.order, backend)
		out = append(out, backend)
	}
	return out, nil
}

func parseBackend(entry rawBackend, prefix string, index int) (Backend, error) {
	if len(entry.Match) == 0 {
		return Backend{}, fmt.Errorf("%sbackends[%d].match must not be empty", prefix, index)
	}
	match := make(map[string]string, len(entry.Match))
	for key, value := range entry.Match {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			return Backend{}, fmt.Errorf("%sbackends[%d].match has an empty attribute key", prefix, index)
		}
		if value == "" {
			return Backend{}, fmt.Errorf("%sbackends[%d].match[%q] must not be empty", prefix, index, key)
		}
		if _, dup := match[key]; dup {
			return Backend{}, fmt.Errorf("%sbackends[%d].match has duplicate attribute key %q", prefix, index, key)
		}
		match[key] = value
	}
	if entry.MaxConcurrency < 1 {
		return Backend{}, fmt.Errorf("%sbackends[%d].max_concurrency must be greater than zero: got %d", prefix, index, entry.MaxConcurrency)
	}
	return Backend{Match: match, MaxConcurrency: entry.MaxConcurrency}, nil
}

// assignBackendNames gives every backend a short display name. A single-key
// match uses the bare value, which reads as "gpu-box-a" for the common
// compat_name setup. Any collision falls back to the fully qualified
// key=value form for every backend so names stay unique.
func assignBackendNames(backends []Backend) {
	names := make([]string, len(backends))
	seen := make(map[string]struct{}, len(backends))
	collision := false
	for index, backend := range backends {
		name := joinMatch(backend.Match, ",")
		if len(backend.Match) == 1 {
			for _, value := range backend.Match {
				name = value
			}
		}
		if _, dup := seen[name]; dup {
			collision = true
		}
		seen[name] = struct{}{}
		names[index] = name
	}
	for index := range backends {
		if collision {
			backends[index].Name = joinMatch(backends[index].Match, ",")
			continue
		}
		backends[index].Name = names[index]
	}
}

// joinMatch renders a match set as sorted key=value pairs so the result is
// stable regardless of YAML map iteration order.
func joinMatch(match map[string]string, separator string) string {
	pairs := make([]string, 0, len(match))
	for key, value := range match {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, separator)
}
