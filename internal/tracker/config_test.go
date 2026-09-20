package tracker

import (
	"testing"
	"time"
)

// shorthandLockConfigYAML is the compatibility lock: the shorthand form the
// plugin shipped with before pools existed. It must keep parsing to one pool
// with the same fill order and limits.
const shorthandLockConfigYAML = `enabled: true
priority: 1
group_aliases: ["local-llm-group"]
on_saturated: least-loaded
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 8
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`

func TestParseConfigLocksTheShorthandForm(t *testing.T) {
	cfg, err := ParseConfig([]byte(shorthandLockConfigYAML))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	if len(cfg.Pools) != 1 {
		t.Fatalf("pools = %d, want exactly one", len(cfg.Pools))
	}
	pool := cfg.Pools[0]
	if len(pool.GroupAliases) != 1 || pool.GroupAliases[0] != "local-llm-group" {
		t.Fatalf("pool aliases = %v", pool.GroupAliases)
	}
	if pool.OnSaturated != PolicyLeastLoaded {
		t.Fatalf("pool policy = %q, want least-loaded", pool.OnSaturated)
	}
	if pool.Name != "local-llm-group" {
		t.Fatalf("pool name = %q", pool.Name)
	}
	if len(pool.Backends) != 2 {
		t.Fatalf("pool backends = %+v", pool.Backends)
	}
	if pool.Backends[0].Name != "gpu-box-a" || pool.Backends[0].MaxConcurrency != 8 {
		t.Fatalf("first backend = %+v", pool.Backends[0])
	}
	if pool.Backends[1].Name != "gpu-box-b" || pool.Backends[1].MaxConcurrency != 2 {
		t.Fatalf("second backend = %+v", pool.Backends[1])
	}

	if cfg.OnSaturated != PolicyLeastLoaded {
		t.Fatalf("top-level policy = %q", cfg.OnSaturated)
	}
	if cfg.PendingTTL != 5*time.Second {
		t.Fatalf("pending TTL = %v", cfg.PendingTTL)
	}
	if got := cfg.GroupAliases(); len(got) != 1 || got[0] != "local-llm-group" {
		t.Fatalf("config aliases = %v", got)
	}
	if len(cfg.Backends) != 2 || cfg.Backends[0].Name != "gpu-box-a" || cfg.Backends[1].Name != "gpu-box-b" {
		t.Fatalf("config backends = %+v", cfg.Backends)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
group_aliases: ["  Local-LLM-Group  ", "", "local-llm-group"]
backends:
  - match: { Compat_Name: " Gpu-Box-A " }
    max_concurrency: 1
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.Pools[0].GroupAliases; len(got) != 1 || got[0] != "local-llm-group" {
		t.Fatalf("group aliases = %v", got)
	}
	if cfg.OnSaturated != PolicyLeastLoaded || cfg.Pools[0].OnSaturated != PolicyLeastLoaded {
		t.Fatalf("policy defaults = %q / %q", cfg.OnSaturated, cfg.Pools[0].OnSaturated)
	}
	if cfg.PendingTTL != DefaultPendingTTL {
		t.Fatalf("pending TTL default = %v", cfg.PendingTTL)
	}
	if got := cfg.Backends[0].Match["compat_name"]; got != "gpu-box-a" {
		t.Fatalf("normalized match = %v", cfg.Backends[0].Match)
	}
}

func TestParseConfigPools(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
enabled: true
on_saturated: first
pending_ttl_ms: 250
pools:
  - group_aliases: ["fast-group", "Fast-Alias"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 8
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 2
  - group_aliases: ["big-group"]
    on_saturated: reject
    backends:
      - match: { compat_name: "gpu-box-c" }
        max_concurrency: 1
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Pools) != 2 {
		t.Fatalf("pools = %d", len(cfg.Pools))
	}
	if got := cfg.Pools[0].GroupAliases; len(got) != 2 || got[0] != "fast-group" || got[1] != "fast-alias" {
		t.Fatalf("pool 0 aliases = %v", got)
	}
	// An unset pool policy inherits the top-level default.
	if cfg.Pools[0].OnSaturated != PolicyFirst {
		t.Fatalf("pool 0 policy = %q, want first", cfg.Pools[0].OnSaturated)
	}
	// An explicit pool policy overrides it.
	if cfg.Pools[1].OnSaturated != PolicyReject {
		t.Fatalf("pool 1 policy = %q, want reject", cfg.Pools[1].OnSaturated)
	}
	if cfg.PendingTTL != 250*time.Millisecond {
		t.Fatalf("pending TTL = %v", cfg.PendingTTL)
	}
	if len(cfg.Backends) != 3 {
		t.Fatalf("backends = %+v", cfg.Backends)
	}
	if got := cfg.GroupAliases(); len(got) != 3 {
		t.Fatalf("all aliases = %v", got)
	}
}

func TestParseConfigSharedBackendAcrossPools(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 4
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 2
  - group_aliases: ["group-b"]
    backends:
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 2
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// gpu-box-b is one physical backend, so it is listed once overall even
	// though two pools reference it.
	if len(cfg.Backends) != 2 {
		t.Fatalf("distinct backends = %+v", cfg.Backends)
	}
	if cfg.Pools[1].Backends[0].Signature() != cfg.Backends[1].Signature() {
		t.Fatal("shared backend does not resolve to the same signature")
	}
	if cfg.Pools[1].Backends[0].Name != "gpu-box-b" {
		t.Fatalf("shared backend name = %q", cfg.Pools[1].Backends[0].Name)
	}
}

func TestParseConfigReservedMatchKeys(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
group_aliases: ["local-llm-group"]
backends:
  - match: { provider: "openai-compatibility", compat_name: "gpu-box-a" }
    max_concurrency: 2
  - match: { auth_id: "Auth-B" }
    max_concurrency: 1
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.Backends[0].Match[MatchKeyProvider]; got != "openai-compatibility" {
		t.Fatalf("provider match = %q", got)
	}
	if got := cfg.Backends[1].Match[MatchKeyAuthID]; got != "auth-b" {
		t.Fatalf("auth_id match = %q", got)
	}
}

func TestParseConfigNamesCollidingBackendsByFullMatch(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { base_url: "gpu-box-a" }
    max_concurrency: 1
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Backends[0].Name != "compat_name=gpu-box-a" || cfg.Backends[1].Name != "base_url=gpu-box-a" {
		t.Fatalf("names = %q, %q", cfg.Backends[0].Name, cfg.Backends[1].Name)
	}
	if cfg.Pools[0].Backends[0].Name != "compat_name=gpu-box-a" {
		t.Fatalf("pool backend name = %q", cfg.Pools[0].Backends[0].Name)
	}
}

func TestParseConfigRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"no group aliases": `
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
`,
		"unknown policy": `
group_aliases: ["local-llm-group"]
on_saturated: sometimes
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
`,
		"unknown pool policy": `
pools:
  - group_aliases: ["group-a"]
    on_saturated: sometimes
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`,
		"zero pending ttl": `
group_aliases: ["local-llm-group"]
pending_ttl_ms: 0
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
`,
		"negative pending ttl": `
group_aliases: ["local-llm-group"]
pending_ttl_ms: -1
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
`,
		"no backends": `
group_aliases: ["local-llm-group"]
`,
		"pool without backends": `
pools:
  - group_aliases: ["group-a"]
`,
		"pool without aliases": `
pools:
  - backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`,
		"both forms at once": `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`,
		"shorthand aliases plus pools": `
group_aliases: ["local-llm-group"]
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`,
		"shorthand backends plus pools": `
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`,
		"alias in two pools": `
pools:
  - group_aliases: ["group-a", "shared"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
  - group_aliases: ["SHARED"]
    backends:
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
`,
		"shared backend with conflicting max": `
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 8
  - group_aliases: ["group-b"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 4
`,
		"empty match": `
group_aliases: ["local-llm-group"]
backends:
  - match: {}
    max_concurrency: 1
`,
		"empty match value": `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "" }
    max_concurrency: 1
`,
		"zero concurrency": `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 0
`,
		"duplicate match in one pool": `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { compat_name: "GPU-BOX-A" }
    max_concurrency: 1
`,
		"malformed yaml": "group_aliases: [\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseConfigRejectsEmptyInput(t *testing.T) {
	if _, err := ParseConfig(nil); err == nil {
		t.Fatal("expected an error for an empty configuration")
	}
}

func TestParseConfigErrorsUseShorthandFieldNames(t *testing.T) {
	_, err := ParseConfig([]byte(`
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 0
`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != `backends[0].max_concurrency must be greater than zero: got 0` {
		t.Fatalf("shorthand error = %q", got)
	}

	_, err = ParseConfig([]byte(`
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 0
`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != `pools[0].backends[0].max_concurrency must be greater than zero: got 0` {
		t.Fatalf("pools error = %q", got)
	}
}
