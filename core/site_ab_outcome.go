package core

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
)

// A/B outcome aggregator — periodically reconstructs the actual whole-house
// behaviour for elapsed run windows and persists via metrics.PersistOutcome.
// Mirrors the goroutine pattern in site_ab_runner.go but on its own cadence
// and with its own mutex / rate limiter.

const (
	abOutcomeInterval     = 15 * time.Minute
	abOutcomeSafetyMargin = 30 * time.Minute // wait for collector writes to settle
	abOutcomeMaxPerTick   = 100              // backlog throttle
)

var (
	abOutcomeUpdated time.Time
	abOutcomeMu      atomic.Uint32
	abOutcomeStarted bool
)

// abRunRequest is the minimal subset of the persisted OptimizationInput JSON
// the aggregator needs. Kept local so the helper doesn't have to track
// optimizer client schema upgrades.
type abRunRequest struct {
	TimeSeries struct {
		Dt []int     `json:"dt"`
		PN []float32 `json:"p_N"`
		PE []float32 `json:"p_E"`
	} `json:"time_series"`
}

// abOutcomeUpdateAsync is the goroutine-safe entry point called from
// site.update(). Mirrors abOptimizerUpdateAsync: rate-limited via
// abOutcomeUpdated, mutex-guarded against concurrent runs, panic-recovered.
func (site *Site) abOutcomeUpdateAsync() {
	if time.Since(abOutcomeUpdated) < abOutcomeInterval {
		return
	}

	if !abOutcomeMu.CompareAndSwap(0, 1) {
		return
	}

	defer func() {
		abOutcomeUpdated = time.Now()
		abOutcomeMu.Store(0)

		if r := recover(); r != nil {
			site.log.ERROR.Printf("ab outcome: panic %v", r)
		}
	}()

	if !abOutcomeStarted {
		abOutcomeStarted = true
		site.log.INFO.Printf("ab outcome: aggregator active (interval=%s, max/tick=%d)",
			abOutcomeInterval, abOutcomeMaxPerTick)
	}

	n, err := site.abOutcomeUpdate()
	if err != nil {
		site.log.ERROR.Printf("ab outcome: %v", err)
	}
	if n > 0 {
		site.log.INFO.Printf("ab outcome: persisted %d new outcomes", n)
	}
}

// abOutcomeUpdate scans pending runs whose window ended at least
// abOutcomeSafetyMargin ago and persists an outcome for each. Per-run errors
// are logged but never abort the loop — one bad run must not block the rest
// of the backlog.
func (site *Site) abOutcomeUpdate() (int, error) {
	cutoff := time.Now().Add(-abOutcomeSafetyMargin)

	pending, err := metrics.PendingRuns(cutoff, abOutcomeMaxPerTick)
	if err != nil {
		return 0, fmt.Errorf("list pending runs: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	var processed int
	for _, run := range pending {
		outcome, err := site.computeOutcomeForRun(run)
		if err != nil {
			site.log.ERROR.Printf("ab outcome: compute run %d: %v", run.ID, err)
			continue
		}
		if err := metrics.PersistOutcome(outcome); err != nil {
			site.log.ERROR.Printf("ab outcome: persist run %d: %v", run.ID, err)
			continue
		}
		processed++
	}
	return processed, nil
}

// computeOutcomeForRun unmarshals the persisted request, queries every
// collector group for the run's window, and returns the resulting AbOutcome.
// Pure orchestration — all math sits in metrics.AggregateOutcome.
func (site *Site) computeOutcomeForRun(run metrics.AbRun) (metrics.AbOutcome, error) {
	var req abRunRequest
	if err := json.Unmarshal([]byte(run.Request), &req); err != nil {
		// Don't fail outright — write a notes-only row so the gap is visible
		// in later analysis and we don't keep retrying.
		return metrics.AbOutcome{
			RunID:       run.ID,
			WindowStart: run.Timestamp,
			WindowEnd:   run.Timestamp.Add(time.Duration(run.Horizon*run.SlotDurationS) * time.Second),
			Notes:       fmt.Sprintf("unmarshal request: %v", err),
		}, nil
	}

	windowStart := run.Timestamp
	windowEnd := windowStart.Add(time.Duration(run.Horizon*run.SlotDurationS) * time.Second)

	flows := metrics.HouseFlows{}
	var qerrs []string
	for _, q := range []struct {
		group string
		dst   *[]metrics.SlotEnergy
	}{
		{metrics.Grid, &flows.Grid},
		{metrics.PV, &flows.PV},
		{metrics.Battery, &flows.Battery},
		{metrics.Home, &flows.Home},
		{metrics.Loadpoint, &flows.Loadpoint},
	} {
		slots, err := metrics.SlotsForGroup(q.group, windowStart, windowEnd)
		if err != nil {
			qerrs = append(qerrs, fmt.Sprintf("query %s: %v", q.group, err))
			continue
		}
		*q.dst = slots
	}

	out := metrics.AggregateOutcome(run, flows, req.TimeSeries.PN, req.TimeSeries.PE)
	if len(qerrs) > 0 {
		if out.Notes != "" {
			out.Notes += "; "
		}
		out.Notes += fmt.Sprintf("query errors: %v", qerrs)
	}
	return out, nil
}
