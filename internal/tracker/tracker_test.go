package tracker

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock drives pending-reservation expiry without wall-clock sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// groupAlias is the steered alias used across these tests. Binds pass it as
// both the execution model and the requested model unless a case is explicitly
// about direct-alias traffic.
const groupAlias = "local-llm-group"

// baseConfigYAML is the locked shorthand fixture from config_test.go, so the
// behavioural tests below run against exactly the configuration the
// compatibility test pins.
const baseConfigYAML = shorthandLockConfigYAML

func mustParse(t *testing.T, raw string) Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func newTestTracker(t *testing.T, raw string) (*Tracker, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	return New(mustParse(t, raw), clock.Now), clock
}

func groupCandidates() []Candidate {
	return []Candidate{
		{AuthID: "auth-a", Attributes: map[string]string{"compat_name": "gpu-box-a", "base_url": "http://gpu-box-a.internal:8000/v1"}},
		{AuthID: "auth-b", Attributes: map[string]string{"compat_name": "gpu-box-b", "base_url": "http://gpu-box-b.internal:8000/v1"}},
	}
}

func backendStatus(t *testing.T, tr *Tracker, name string) BackendStatus {
	t.Helper()
	for _, entry := range tr.Status().Backends {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("backend %q missing from status", name)
	return BackendStatus{}
}

// pickAndBind performs one full pick-then-bind cycle and returns the backend
// that took the request.
func pickAndBind(t *testing.T, tr *Tracker, requestID string) string {
	t.Helper()
	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if !decision.Handled {
		t.Fatalf("pick for %s was not handled: %+v", requestID, decision)
	}
	tr.Bind(requestID, decision.AuthID, groupAlias, groupAlias)
	return decision.Backend
}

func TestPickFillsBackendsInConfigOrder(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	for i := 0; i < 8; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("req-%d", i)); got != "gpu-box-a" {
			t.Fatalf("request %d went to %q, want gpu-box-a", i, got)
		}
	}
	for i := 8; i < 10; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("req-%d", i)); got != "gpu-box-b" {
			t.Fatalf("request %d went to %q, want gpu-box-b", i, got)
		}
	}

	boxA := backendStatus(t, tr, "gpu-box-a")
	boxB := backendStatus(t, tr, "gpu-box-b")
	if boxA.Inflight != 8 || boxA.Pending != 0 || boxA.Available != 0 {
		t.Fatalf("gpu-box-a status = %+v", boxA)
	}
	if boxB.Inflight != 2 || boxB.Pending != 0 || boxB.Available != 0 {
		t.Fatalf("gpu-box-b status = %+v", boxB)
	}

	totals := tr.Status().Totals
	if totals.Inflight != 10 || totals.MaxConcurrency != 10 || totals.Available != 0 || totals.TrackedRequests != 10 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestPickReservesPendingSlotsBeforeBind(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	for i := 0; i < 8; i++ {
		if decision := tr.Pick("local-llm-group", "", groupCandidates()); decision.Backend != "gpu-box-a" {
			t.Fatalf("pick %d went to %q, want gpu-box-a", i, decision.Backend)
		}
	}
	// Nothing has bound yet, so the reservations alone must push the next pick
	// onto the spill-over backend.
	if decision := tr.Pick("local-llm-group", "", groupCandidates()); decision.Backend != "gpu-box-b" {
		t.Fatalf("ninth pick went to %q, want gpu-box-b", decision.Backend)
	}
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 8 || boxA.Inflight != 0 {
		t.Fatalf("gpu-box-a status = %+v", boxA)
	}
}

func TestSaturatedLeastLoadedPicksLowestRatio(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
on_saturated: least-loaded
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 2
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`)
	// Direct-alias traffic can push a backend past its limit, which is the only
	// way the two ratios differ once both are saturated.
	tr.Pick("gpu-box-a", "", groupCandidates())
	for i := 0; i < 3; i++ {
		tr.Bind(fmt.Sprintf("direct-%d", i), "auth-a", "gpu-box-a", "gpu-box-a")
	}
	for i := 0; i < 2; i++ {
		pickAndBind(t, tr, fmt.Sprintf("group-%d", i))
	}

	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 3 {
		t.Fatalf("gpu-box-a status = %+v", boxA)
	}
	if boxB := backendStatus(t, tr, "gpu-box-b"); boxB.Inflight != 2 {
		t.Fatalf("gpu-box-b status = %+v", boxB)
	}

	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if !decision.Handled || decision.Backend != "gpu-box-b" {
		t.Fatalf("saturated pick = %+v, want handled gpu-box-b", decision)
	}
}

func TestSaturatedLeastLoadedBreaksTiesInConfigOrder(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)
	for i := 0; i < 10; i++ {
		pickAndBind(t, tr, fmt.Sprintf("req-%d", i))
	}
	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if !decision.Handled || decision.Backend != "gpu-box-a" {
		t.Fatalf("tie-break pick = %+v, want handled gpu-box-a", decision)
	}
}

func TestSaturatedFirstPicksFirstBackend(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
on_saturated: first
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 1
`)
	pickAndBind(t, tr, "req-0")
	pickAndBind(t, tr, "req-1")

	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if !decision.Handled || decision.Backend != "gpu-box-a" || decision.AuthID != "auth-a" {
		t.Fatalf("saturated pick = %+v, want handled gpu-box-a", decision)
	}
}

func TestSaturatedRejectReportsRejection(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
on_saturated: reject
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 1
`)
	pickAndBind(t, tr, "req-0")
	pickAndBind(t, tr, "req-1")

	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if decision.Handled || !decision.Rejected {
		t.Fatalf("saturated pick = %+v, want rejected", decision)
	}
}

func TestConcurrentPicksDoNotOversubscribe(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	const picks = 10
	results := make(chan Decision, picks)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < picks; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			decision := tr.Pick("local-llm-group", "", groupCandidates())
			if decision.Handled {
				tr.Bind(fmt.Sprintf("req-%d", index), decision.AuthID, groupAlias, groupAlias)
			}
			results <- decision
		}(i)
	}
	start.Done()
	done.Wait()
	close(results)

	counts := map[string]int{}
	for decision := range results {
		if !decision.Handled {
			t.Fatalf("concurrent pick was not handled: %+v", decision)
		}
		counts[decision.Backend]++
	}
	if counts["gpu-box-a"] != 8 || counts["gpu-box-b"] != 2 {
		t.Fatalf("pick distribution = %v, want 8 gpu-box-a and 2 gpu-box-b", counts)
	}
	totals := tr.Status().Totals
	if totals.Inflight != 10 || totals.Pending != 0 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestPendingReservationExpires(t *testing.T) {
	tr, clock := newTestTracker(t, baseConfigYAML)

	for i := 0; i < 8; i++ {
		tr.Pick("local-llm-group", "", groupCandidates())
	}
	if decision := tr.Pick("local-llm-group", "", groupCandidates()); decision.Backend != "gpu-box-b" {
		t.Fatalf("pick before expiry went to %q, want gpu-box-b", decision.Backend)
	}

	clock.Advance(4999 * time.Millisecond)
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 8 {
		t.Fatalf("gpu-box-a pending before TTL = %d, want 8", boxA.Pending)
	}

	clock.Advance(2 * time.Millisecond)
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 0 {
		t.Fatalf("gpu-box-a pending after TTL = %d, want 0", boxA.Pending)
	}
	if decision := tr.Pick("local-llm-group", "", groupCandidates()); decision.Backend != "gpu-box-a" {
		t.Fatalf("pick after expiry went to %q, want gpu-box-a", decision.Backend)
	}
}

func TestBindConsumesOnePendingReservation(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	decision := tr.Pick("local-llm-group", "", groupCandidates())
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 1 {
		t.Fatalf("gpu-box-a pending after pick = %d, want 1", boxA.Pending)
	}
	tr.Bind("req-0", decision.AuthID, groupAlias, groupAlias)
	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Pending != 0 || boxA.Inflight != 1 {
		t.Fatalf("gpu-box-a status after bind = %+v", boxA)
	}
}

func TestRetryRebindReleasesPreviousBackend(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	tr.Pick("local-llm-group", "", groupCandidates())
	tr.Bind("req-0", "auth-a", groupAlias, groupAlias)
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 1 {
		t.Fatalf("gpu-box-a inflight after first bind = %d, want 1", boxA.Inflight)
	}

	// The host retries the same request on the other credential.
	tr.Bind("req-0", "auth-b", groupAlias, groupAlias)
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 0 {
		t.Fatalf("gpu-box-a inflight after retry = %d, want 0", boxA.Inflight)
	}
	if boxB := backendStatus(t, tr, "gpu-box-b"); boxB.Inflight != 1 {
		t.Fatalf("gpu-box-b inflight after retry = %d, want 1", boxB.Inflight)
	}
	if tracked := tr.Status().Totals.TrackedRequests; tracked != 1 {
		t.Fatalf("tracked requests = %d, want 1", tracked)
	}

	tr.Complete("req-0")
	if totals := tr.Status().Totals; totals.Inflight != 0 || totals.TrackedRequests != 0 {
		t.Fatalf("totals after complete = %+v", totals)
	}
}

func TestRebindToSameBackendIsIdempotent(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	tr.Pick("local-llm-group", "", groupCandidates())
	tr.Bind("req-0", "auth-a", groupAlias, groupAlias)
	tr.Bind("req-0", "auth-a", groupAlias, groupAlias)
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 1 {
		t.Fatalf("gpu-box-a inflight after repeated bind = %d, want 1", boxA.Inflight)
	}
}

func TestCompleteIsIdempotentAndIgnoresUnknownRequests(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	pickAndBind(t, tr, "req-0")
	tr.Complete("req-0")
	tr.Complete("req-0")
	tr.Complete("never-seen")
	tr.Complete("")

	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 0 || boxA.Available != 8 {
		t.Fatalf("gpu-box-a status = %+v", boxA)
	}
	if totals := tr.Status().Totals; totals.Inflight != 0 || totals.TrackedRequests != 0 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestDirectAliasTrafficCountsAgainstBackendLimit(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
on_saturated: least-loaded
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 2
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`)

	// A request for the direct alias "gpu-box-a" is not steered, but the pick still
	// teaches the tracker which backend owns the candidate auth IDs.
	direct := tr.Pick("gpu-box-a", "", groupCandidates())
	if direct.Handled {
		t.Fatalf("direct alias pick = %+v, want unhandled", direct)
	}
	tr.Bind("direct-0", "auth-a", "gpu-box-a", "gpu-box-a")
	tr.Bind("direct-1", "auth-a", "gpu-box-a", "gpu-box-a")
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 2 {
		t.Fatalf("gpu-box-a inflight = %d, want 2", boxA.Inflight)
	}

	// gpu-box-a is now full purely from direct traffic, so group traffic spills over.
	if got := pickAndBind(t, tr, "group-0"); got != "gpu-box-b" {
		t.Fatalf("group pick went to %q, want gpu-box-b", got)
	}
}

func TestReconfigureKeepsInflightStateAndAppliesNewLimits(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	for i := 0; i < 3; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("req-%d", i)); got != "gpu-box-a" {
			t.Fatalf("request %d went to %q, want gpu-box-a", i, got)
		}
	}

	tr.Reconfigure(mustParse(t, `
group_aliases: ["local-llm-group"]
on_saturated: least-loaded
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 4
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 1
`))

	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 3 || boxA.MaxConcurrency != 4 || boxA.Available != 1 {
		t.Fatalf("gpu-box-a status after reconfigure = %+v", boxA)
	}
	if got := pickAndBind(t, tr, "req-3"); got != "gpu-box-a" {
		t.Fatalf("request 3 went to %q, want gpu-box-a", got)
	}
	if got := pickAndBind(t, tr, "req-4"); got != "gpu-box-b" {
		t.Fatalf("request 4 went to %q, want gpu-box-b", got)
	}

	// Completing a request bound before the reconfigure still releases its slot.
	tr.Complete("req-0")
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 3 {
		t.Fatalf("gpu-box-a inflight after complete = %d, want 3", boxA.Inflight)
	}
}

func TestReconfigureDropsRemovedBackends(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)
	pickAndBind(t, tr, "req-0")

	tr.Reconfigure(mustParse(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`))

	status := tr.Status()
	if len(status.Backends) != 1 || status.Backends[0].Name != "gpu-box-b" {
		t.Fatalf("backends after reconfigure = %+v", status.Backends)
	}
	if status.Totals.Inflight != 0 || status.Totals.TrackedRequests != 0 {
		t.Fatalf("totals after reconfigure = %+v", status.Totals)
	}
	// The stale completion must not underflow the surviving backend.
	tr.Complete("req-0")
	if boxB := backendStatus(t, tr, "gpu-box-b"); boxB.Inflight != 0 {
		t.Fatalf("gpu-box-b inflight = %d, want 0", boxB.Inflight)
	}
}

func TestPickIgnoresNonGroupModel(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	decision := tr.Pick("gpt-4o", "gpt-4o", groupCandidates())
	if decision.Handled || decision.Rejected {
		t.Fatalf("non-group pick = %+v, want unhandled", decision)
	}
	if totals := tr.Status().Totals; totals.Pending != 0 {
		t.Fatalf("non-group pick reserved a slot: %+v", totals)
	}
}

func TestPickUsesRequestedModelWhenModelIsRewritten(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	decision := tr.Pick("model-a", "local-llm-group", groupCandidates())
	if !decision.Handled || decision.Backend != "gpu-box-a" {
		t.Fatalf("pick = %+v, want handled gpu-box-a", decision)
	}
}

func TestPickMatchesAliasesCaseInsensitively(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	decision := tr.Pick("Local-LLM-Group", "", []Candidate{
		{AuthID: "auth-a", Attributes: map[string]string{"Compat_Name": "GPU-BOX-A"}},
	})
	if !decision.Handled || decision.Backend != "gpu-box-a" {
		t.Fatalf("pick = %+v, want handled gpu-box-a", decision)
	}
}

func TestPickUnhandledWhenNoCandidateMatches(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	decision := tr.Pick("local-llm-group", "", []Candidate{
		{AuthID: "auth-other", Attributes: map[string]string{"compat_name": "elsewhere"}},
	})
	if decision.Handled || decision.Rejected {
		t.Fatalf("pick = %+v, want unhandled", decision)
	}
}

func TestPickRequiresEveryMatchAttribute(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
backends:
  - match:
      compat_name: "gpu-box-a"
      provider_key: "openai-compatibility"
    max_concurrency: 1
`)

	if decision := tr.Pick("local-llm-group", "", []Candidate{
		{AuthID: "auth-a", Attributes: map[string]string{"compat_name": "gpu-box-a"}},
	}); decision.Handled {
		t.Fatalf("partial attribute match was handled: %+v", decision)
	}
	if decision := tr.Pick("local-llm-group", "", []Candidate{
		{AuthID: "auth-a", Attributes: map[string]string{"compat_name": "gpu-box-a", "provider_key": "openai-compatibility"}},
	}); !decision.Handled {
		t.Fatalf("full attribute match was not handled")
	}
}

func TestBindIgnoresUnknownAuthAndReleasesPreviousSlot(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	pickAndBind(t, tr, "req-0")
	tr.Bind("req-0", "auth-unknown", groupAlias, groupAlias)

	totals := tr.Status().Totals
	if totals.Inflight != 0 || totals.TrackedRequests != 0 {
		t.Fatalf("totals = %+v", totals)
	}
	tr.Bind("", "auth-a", groupAlias, groupAlias)
	if tracked := tr.Status().Totals.TrackedRequests; tracked != 0 {
		t.Fatalf("empty request ID was tracked: %d", tracked)
	}
}

func TestDirectAliasBindDoesNotConsumeGroupReservation(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	// A steered pick reserves a slot on gpu-box-a for a group request that has not
	// reached request.intercept_after yet.
	decision := tr.Pick(groupAlias, "", groupCandidates())
	if decision.Backend != "gpu-box-a" {
		t.Fatalf("group pick went to %q, want gpu-box-a", decision.Backend)
	}
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 1 || boxA.Inflight != 0 {
		t.Fatalf("gpu-box-a status after pick = %+v", boxA)
	}

	// A direct-alias request binds to the same backend. It never created a
	// reservation, so it must take an in-flight slot and leave the group
	// reservation alone.
	tr.Bind("direct-0", "auth-a", "model-a", "gpu-box-a")
	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 1 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status after direct bind = %+v", boxA)
	}

	// The group request then claims the reservation it made.
	tr.Bind("group-0", decision.AuthID, groupAlias, groupAlias)
	boxA = backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 2 || boxA.Pending != 0 {
		t.Fatalf("gpu-box-a status after group bind = %+v", boxA)
	}
}

// TestDirectAliasBindDoesNotFreeCapacityForGroupPick is the regression test for
// the reservation-stealing defect. Consuming the group reservation on a
// direct-alias bind leaves the backend one slot short of its true load, so the
// next group pick stays on a backend that is already committed to its limit.
func TestDirectAliasBindDoesNotFreeCapacityForGroupPick(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	// Six bound group requests plus one outstanding reservation put gpu-box-a at a
	// load of seven against its limit of eight.
	for i := 0; i < 6; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("group-%d", i)); got != "gpu-box-a" {
			t.Fatalf("group request %d went to %q, want gpu-box-a", i, got)
		}
	}
	if reserved := tr.Pick(groupAlias, "", groupCandidates()); reserved.Backend != "gpu-box-a" {
		t.Fatalf("reserving pick went to %q, want gpu-box-a", reserved.Backend)
	}
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 6 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status before direct bind = %+v", boxA)
	}

	// The direct-alias bind raises the load to eight. It must not consume the
	// group reservation, which would report a load of seven.
	tr.Bind("direct-0", "auth-a", "model-a", "gpu-box-a")
	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 7 || boxA.Pending != 1 || boxA.Available != 0 {
		t.Fatalf("gpu-box-a status after direct bind = %+v", boxA)
	}

	if decision := tr.Pick(groupAlias, "", groupCandidates()); decision.Backend != "gpu-box-b" {
		t.Fatalf("group pick after direct bind went to %q, want gpu-box-b", decision.Backend)
	}
}

// TestDirectAliasBindAtLimitKeepsGroupPickOnNextBackend covers the same defect
// one slot further along, where gpu-box-a is already at its limit.
func TestDirectAliasBindAtLimitKeepsGroupPickOnNextBackend(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	for i := 0; i < 7; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("group-%d", i)); got != "gpu-box-a" {
			t.Fatalf("group request %d went to %q, want gpu-box-a", i, got)
		}
	}
	if reserved := tr.Pick(groupAlias, "", groupCandidates()); reserved.Backend != "gpu-box-a" {
		t.Fatalf("reserving pick went to %q, want gpu-box-a", reserved.Backend)
	}
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 7 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status before direct bind = %+v", boxA)
	}

	tr.Bind("direct-0", "auth-a", "model-a", "gpu-box-a")
	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 8 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status after direct bind = %+v", boxA)
	}

	if decision := tr.Pick(groupAlias, "", groupCandidates()); decision.Backend != "gpu-box-b" {
		t.Fatalf("group pick after direct bind went to %q, want gpu-box-b", decision.Backend)
	}
}

func TestGroupBindConsumesExactlyOneReservation(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)

	first := tr.Pick(groupAlias, "", groupCandidates())
	tr.Pick(groupAlias, "", groupCandidates())
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Pending != 2 {
		t.Fatalf("gpu-box-a pending after two picks = %d, want 2", boxA.Pending)
	}

	tr.Bind("group-0", first.AuthID, groupAlias, groupAlias)
	boxA := backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 1 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status after one bind = %+v", boxA)
	}

	// Repeating the same bind must not consume the second reservation.
	tr.Bind("group-0", first.AuthID, groupAlias, groupAlias)
	boxA = backendStatus(t, tr, "gpu-box-a")
	if boxA.Inflight != 1 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status after repeated bind = %+v", boxA)
	}
}

func TestRetryRebindConsumesReservationOnNewBackend(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`)

	first := tr.Pick(groupAlias, "", groupCandidates())
	if first.Backend != "gpu-box-a" {
		t.Fatalf("first pick went to %q, want gpu-box-a", first.Backend)
	}
	tr.Bind("req-0", first.AuthID, groupAlias, groupAlias)

	// The host retries the request; the fresh pick reserves a slot on gpu-box-b.
	retry := tr.Pick(groupAlias, "", groupCandidates())
	if retry.Backend != "gpu-box-b" {
		t.Fatalf("retry pick went to %q, want gpu-box-b", retry.Backend)
	}
	if boxB := backendStatus(t, tr, "gpu-box-b"); boxB.Pending != 1 {
		t.Fatalf("gpu-box-b pending after retry pick = %d, want 1", boxB.Pending)
	}

	tr.Bind("req-0", retry.AuthID, groupAlias, groupAlias)
	boxA := backendStatus(t, tr, "gpu-box-a")
	boxB := backendStatus(t, tr, "gpu-box-b")
	if boxA.Inflight != 0 || boxA.Pending != 0 {
		t.Fatalf("gpu-box-a status after retry bind = %+v", boxA)
	}
	if boxB.Inflight != 1 || boxB.Pending != 0 {
		t.Fatalf("gpu-box-b status after retry bind = %+v", boxB)
	}
}

func poolCandidates() []Candidate {
	return []Candidate{
		{AuthID: "auth-a", Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "gpu-box-a"}},
		{AuthID: "auth-b", Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "gpu-box-b"}},
		{AuthID: "auth-c", Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "gpu-box-c"}},
	}
}

func TestPoolsRouteAliasesIndependently(t *testing.T) {
	tr, _ := newTestTracker(t, `
pools:
  - group_aliases: ["fast-group"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
  - group_aliases: ["big-group"]
    backends:
      - match: { compat_name: "gpu-box-c" }
        max_concurrency: 1
`)

	fast := tr.Pick("fast-group", "", poolCandidates())
	if !fast.Handled || fast.Backend != "gpu-box-a" || fast.Pool != "fast-group" {
		t.Fatalf("fast pick = %+v", fast)
	}
	big := tr.Pick("big-group", "", poolCandidates())
	if !big.Handled || big.Backend != "gpu-box-c" || big.Pool != "big-group" {
		t.Fatalf("big pick = %+v", big)
	}

	// A pool never spills onto a backend outside its own fill order.
	tr.Bind("big-0", big.AuthID, "big-group", "big-group")
	saturated := tr.Pick("big-group", "", poolCandidates())
	if saturated.Backend != "gpu-box-c" {
		t.Fatalf("saturated big pick went to %q, want gpu-box-c", saturated.Backend)
	}
}

func TestPoolsShareOneBackendCounter(t *testing.T) {
	tr, _ := newTestTracker(t, `
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-shared" }
        max_concurrency: 2
      - match: { compat_name: "gpu-box-a-only" }
        max_concurrency: 4
  - group_aliases: ["group-b"]
    backends:
      - match: { compat_name: "gpu-box-shared" }
        max_concurrency: 2
      - match: { compat_name: "gpu-box-b-only" }
        max_concurrency: 4
`)
	candidates := []Candidate{
		{AuthID: "auth-shared", Attributes: map[string]string{"compat_name": "gpu-box-shared"}},
		{AuthID: "auth-a", Attributes: map[string]string{"compat_name": "gpu-box-a-only"}},
		{AuthID: "auth-b", Attributes: map[string]string{"compat_name": "gpu-box-b-only"}},
	}

	// One physical backend, one entry in the report.
	if backends := tr.Status().Backends; len(backends) != 3 {
		t.Fatalf("distinct backends = %d, want 3", len(backends))
	}

	// One request through each pool fills the shared backend to its limit of 2.
	a := tr.Pick("group-a", "", candidates)
	if a.Backend != "gpu-box-shared" {
		t.Fatalf("group-a pick went to %q", a.Backend)
	}
	tr.Bind("a-0", a.AuthID, "group-a", "group-a")

	b := tr.Pick("group-b", "", candidates)
	if b.Backend != "gpu-box-shared" {
		t.Fatalf("group-b pick went to %q", b.Backend)
	}
	tr.Bind("b-0", b.AuthID, "group-b", "group-b")

	shared := backendStatus(t, tr, "gpu-box-shared")
	if shared.Inflight != 2 || shared.Available != 0 {
		t.Fatalf("shared backend status = %+v", shared)
	}

	// Load put on the shared backend through one pool is visible to the other.
	if next := tr.Pick("group-b", "", candidates); next.Backend != "gpu-box-b-only" {
		t.Fatalf("group-b spill went to %q, want gpu-box-b-only", next.Backend)
	}
	if next := tr.Pick("group-a", "", candidates); next.Backend != "gpu-box-a-only" {
		t.Fatalf("group-a spill went to %q, want gpu-box-a-only", next.Backend)
	}

	// Completing through one pool frees the slot for the other.
	tr.Complete("a-0")
	if shared := backendStatus(t, tr, "gpu-box-shared"); shared.Inflight != 1 {
		t.Fatalf("shared inflight after complete = %d, want 1", shared.Inflight)
	}
}

func TestPoolPolicyOverridesTopLevel(t *testing.T) {
	tr, _ := newTestTracker(t, `
on_saturated: first
pools:
  - group_aliases: ["inherits"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
  - group_aliases: ["rejects"]
    on_saturated: reject
    backends:
      - match: { compat_name: "gpu-box-c" }
        max_concurrency: 1
`)

	// The inheriting pool takes the top-level "first" policy once saturated.
	first := tr.Pick("inherits", "", poolCandidates())
	tr.Bind("i-0", first.AuthID, "inherits", "inherits")
	second := tr.Pick("inherits", "", poolCandidates())
	tr.Bind("i-1", second.AuthID, "inherits", "inherits")
	saturated := tr.Pick("inherits", "", poolCandidates())
	if !saturated.Handled || saturated.Backend != "gpu-box-a" {
		t.Fatalf("inheriting pool saturated pick = %+v, want handled gpu-box-a", saturated)
	}

	// The overriding pool rejects instead.
	only := tr.Pick("rejects", "", poolCandidates())
	tr.Bind("r-0", only.AuthID, "rejects", "rejects")
	rejected := tr.Pick("rejects", "", poolCandidates())
	if rejected.Handled || !rejected.Rejected || rejected.Pool != "rejects" {
		t.Fatalf("overriding pool saturated pick = %+v, want rejected", rejected)
	}
}

func TestMatchOnProviderKey(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { provider: "openai-compatibility", compat_name: "gpu-box-a" }
    max_concurrency: 1
`)

	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "auth-a", Provider: "OpenAI-Compatibility", Attributes: map[string]string{"compat_name": "gpu-box-a"}},
	}); !decision.Handled || decision.AuthID != "auth-a" {
		t.Fatalf("matching provider pick = %+v", decision)
	}
	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "auth-a", Provider: "gemini", Attributes: map[string]string{"compat_name": "gpu-box-a"}},
	}); decision.Handled {
		t.Fatalf("mismatched provider pick was handled: %+v", decision)
	}
}

func TestMatchOnAuthIDKey(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { auth_id: "auth-pinned" }
    max_concurrency: 1
`)

	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "Auth-Pinned", Provider: "openai-compatibility"},
	}); !decision.Handled || decision.AuthID != "Auth-Pinned" {
		t.Fatalf("matching auth_id pick = %+v", decision)
	}
	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "auth-other", Provider: "openai-compatibility"},
	}); decision.Handled {
		t.Fatalf("mismatched auth_id pick was handled: %+v", decision)
	}
}

func TestReservedMatchKeysShadowAttributes(t *testing.T) {
	tr, _ := newTestTracker(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { provider: "openai-compatibility" }
    max_concurrency: 1
`)

	// An attribute literally named provider must not satisfy the reserved key.
	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "auth-a", Provider: "gemini", Attributes: map[string]string{"provider": "openai-compatibility"}},
	}); decision.Handled {
		t.Fatalf("shadowed attribute satisfied the reserved key: %+v", decision)
	}
	// The candidate field decides.
	if decision := tr.Pick(groupAlias, "", []Candidate{
		{AuthID: "auth-a", Provider: "openai-compatibility", Attributes: map[string]string{"provider": "gemini"}},
	}); !decision.Handled {
		t.Fatal("candidate provider field did not satisfy the reserved key")
	}
}

func TestReconfigureFromShorthandToPoolsKeepsSharedCounters(t *testing.T) {
	tr, _ := newTestTracker(t, baseConfigYAML)
	for i := 0; i < 3; i++ {
		if got := pickAndBind(t, tr, fmt.Sprintf("req-%d", i)); got != "gpu-box-a" {
			t.Fatalf("request %d went to %q, want gpu-box-a", i, got)
		}
	}

	tr.Reconfigure(mustParse(t, `
pools:
  - group_aliases: ["local-llm-group"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 8
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 2
  - group_aliases: ["overflow-group"]
    on_saturated: reject
    backends:
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 2
`))

	// The signature is unchanged, so gpu-box-a keeps its live count.
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 3 {
		t.Fatalf("gpu-box-a inflight after reconfigure = %d, want 3", boxA.Inflight)
	}
	status := tr.Status()
	if len(status.Pools) != 2 || len(status.Backends) != 2 {
		t.Fatalf("status shape after reconfigure: %d pools, %d backends", len(status.Pools), len(status.Backends))
	}
	// A request completing after the reconfigure still releases its slot.
	tr.Complete("req-0")
	if boxA := backendStatus(t, tr, "gpu-box-a"); boxA.Inflight != 2 {
		t.Fatalf("gpu-box-a inflight after complete = %d, want 2", boxA.Inflight)
	}
}

func TestStatusReportsPoolsAndLegacyKeys(t *testing.T) {
	tr, _ := newTestTracker(t, `
on_saturated: first
pending_ttl_ms: 1500
pools:
  - group_aliases: ["group-a", "group-a-alt"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 3
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
  - group_aliases: ["group-b"]
    on_saturated: reject
    backends:
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
`)
	decision := tr.Pick("group-a", "", poolCandidates())
	tr.Bind("a-0", decision.AuthID, "group-a", "group-a")

	status := tr.Status()

	if len(status.Pools) != 2 {
		t.Fatalf("pools = %+v", status.Pools)
	}
	if got := status.Pools[0]; got.Name != "group-a" ||
		len(got.GroupAliases) != 2 || got.GroupAliases[1] != "group-a-alt" ||
		got.OnSaturated != string(PolicyFirst) ||
		len(got.Backends) != 2 || got.Backends[0] != "gpu-box-a" || got.Backends[1] != "gpu-box-b" {
		t.Fatalf("pool 0 = %+v", got)
	}
	if got := status.Pools[1]; got.OnSaturated != string(PolicyReject) ||
		len(got.Backends) != 1 || got.Backends[0] != "gpu-box-b" {
		t.Fatalf("pool 1 = %+v", got)
	}

	// The shared backend is reported once, not once per pool.
	if len(status.Backends) != 2 {
		t.Fatalf("backends = %+v", status.Backends)
	}
	if got := status.Backends[0]; got.Name != "gpu-box-a" || got.MaxConcurrency != 3 || got.Inflight != 1 || got.Available != 2 {
		t.Fatalf("backend 0 = %+v", got)
	}

	// Legacy keys keep working for consumers written before pools existed.
	if status.Totals.MaxConcurrency != 4 || status.Totals.Inflight != 1 || status.Totals.Available != 3 || status.Totals.TrackedRequests != 1 {
		t.Fatalf("totals = %+v", status.Totals)
	}
	if got := status.Config.GroupAliases; len(got) != 3 || got[0] != "group-a" || got[2] != "group-b" {
		t.Fatalf("config aliases = %v", got)
	}
	if status.Config.OnSaturated != string(PolicyFirst) || status.Config.PendingTTLMS != 1500 {
		t.Fatalf("config = %+v", status.Config)
	}
}

func TestPickUnhandledForAliasOutsideEveryPool(t *testing.T) {
	tr, _ := newTestTracker(t, `
pools:
  - group_aliases: ["group-a"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`)
	if decision := tr.Pick("group-b", "group-b", poolCandidates()); decision.Handled || decision.Rejected {
		t.Fatalf("pick for an unconfigured alias = %+v", decision)
	}
	// The mapping is still learned, so direct traffic keeps being counted.
	tr.Bind("direct-0", "auth-a", "gpu-box-a", "gpu-box-a")
	if got := backendStatus(t, tr, "gpu-box-a"); got.Inflight != 1 {
		t.Fatalf("gpu-box-a inflight = %d, want 1", got.Inflight)
	}
}
