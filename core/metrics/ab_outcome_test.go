package metrics

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test fixture helpers ---------------------------------------------------

// newRun creates an AbRun row and returns it freshly fetched (so ID is set).
func newRun(t *testing.T, ts time.Time, horizon int) AbRun {
	t.Helper()
	id, err := PersistRun(ts, horizon, tariff.SlotDuration, `{}`)
	require.NoError(t, err)
	return AbRun{
		ID:            id,
		Timestamp:     ts,
		Horizon:       horizon,
		SlotDurationS: int(tariff.SlotDuration.Seconds()),
	}
}

// insertSlot writes one entity + one meters row for a given group/title/slot.
// Returns the slot timestamp aligned to SlotDuration.
func insertSlot(t *testing.T, group, name string, slot time.Time, energyKwh, returnKwh float64) {
	t.Helper()
	e, err := createEntity(group, name, name)
	require.NoError(t, err)
	require.NoError(t, persist(e, slot, energyKwh, returnKwh))
}

// flatPrices returns a slice of length n where every entry is v. Convenience
// for tests that don't care about per-slot price variation.
func flatPrices(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// PendingRuns ------------------------------------------------------------

func TestPendingRuns_SkipsIncompleteWindow(t *testing.T) {
	setupAbLogDB(t)
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	// window: 4 slots × 15min = 1h ending at +1h
	newRun(t, now, 4)

	// cutoff 30 min in — window not yet ended
	got, err := PendingRuns(now.Add(30*time.Minute), 10)
	require.NoError(t, err)
	assert.Empty(t, got)

	// cutoff 1h+1s in — window ended
	got, err = PendingRuns(now.Add(time.Hour+time.Second), 10)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestPendingRuns_SkipsRunsWithOutcome(t *testing.T) {
	setupAbLogDB(t)
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	r := newRun(t, now, 4)

	require.NoError(t, PersistOutcome(AbOutcome{RunID: r.ID, WindowStart: now}))

	got, err := PendingRuns(now.Add(2*time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPendingRuns_ReturnsOldestFirstWithLimit(t *testing.T) {
	setupAbLogDB(t)
	base := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

	r1 := newRun(t, base, 4)
	r2 := newRun(t, base.Add(time.Hour), 4)
	newRun(t, base.Add(2*time.Hour), 4)

	got, err := PendingRuns(base.Add(24*time.Hour), 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, r1.ID, got[0].ID)
	assert.Equal(t, r2.ID, got[1].ID)
}

// SlotsForGroup ----------------------------------------------------------

func TestSlotsForGroup_SumsAcrossMeters(t *testing.T) {
	setupAbLogDB(t)
	slot := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)

	insertSlot(t, Grid, "grid-1", slot, 1.0, 0.2)
	insertSlot(t, Grid, "grid-2", slot, 0.5, 0.1)

	got, err := SlotsForGroup(Grid, slot.Add(-time.Minute), slot.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 1.5, got[0].Energy, 1e-9)
	assert.InDelta(t, 0.3, got[0].ReturnEnergy, 1e-9)
}

func TestSlotsForGroup_FiltersByTimeRange(t *testing.T) {
	setupAbLogDB(t)
	base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	insertSlot(t, Grid, "g", base, 1.0, 0)
	insertSlot(t, Grid, "g", base.Add(tariff.SlotDuration), 2.0, 0)
	insertSlot(t, Grid, "g", base.Add(2*tariff.SlotDuration), 3.0, 0)

	got, err := SlotsForGroup(Grid, base.Add(tariff.SlotDuration), base.Add(2*tariff.SlotDuration))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 2.0, got[0].Energy, 1e-9)
}

func TestSlotsForGroup_ColumnNames(t *testing.T) {
	// Regression guard: post-0.309.1 columns are energy / return_energy
	// (renamed from import / export). If the query references the old names
	// it'll error or return zeros.
	setupAbLogDB(t)
	slot := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	insertSlot(t, PV, "pv-1", slot, 4.5, 0)

	got, err := SlotsForGroup(PV, slot.Add(-time.Hour), slot.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 4.5, got[0].Energy, 1e-9)
	assert.InDelta(t, 0.0, got[0].ReturnEnergy, 1e-9)
}

// AggregateOutcome -------------------------------------------------------

func TestAggregateOutcome_FullWindowAllGroups(t *testing.T) {
	setupAbLogDB(t)
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 2, SlotDurationS: 900}

	flows := HouseFlows{
		Grid: []SlotEnergy{
			{Start: ws, Energy: 0.5, ReturnEnergy: 0.0},                                       // import 500 Wh
			{Start: ws.Add(tariff.SlotDuration), Energy: 0.0, ReturnEnergy: 0.2},              // export 200 Wh
		},
		PV: []SlotEnergy{
			{Start: ws, Energy: 0.3, ReturnEnergy: 0},
			{Start: ws.Add(tariff.SlotDuration), Energy: 0.4, ReturnEnergy: 0},
		},
		Battery: []SlotEnergy{
			{Start: ws, Energy: 0.1, ReturnEnergy: 0.0},                                       // charge 100 Wh
			{Start: ws.Add(tariff.SlotDuration), Energy: 0.0, ReturnEnergy: 0.05},             // discharge 50 Wh
		},
		Home: []SlotEnergy{
			{Start: ws, Energy: 0.25, ReturnEnergy: 0},
			{Start: ws.Add(tariff.SlotDuration), Energy: 0.30, ReturnEnergy: 0},
		},
		Loadpoint: []SlotEnergy{
			{Start: ws, Energy: 0.0, ReturnEnergy: 0},
			{Start: ws.Add(tariff.SlotDuration), Energy: 1.5, ReturnEnergy: 0},                // 1500 Wh
		},
	}
	// 0.20 EUR/kWh = 0.0002 EUR/Wh import; 0.05 EUR/kWh = 0.00005 EUR/Wh feedin
	priceN := []float32{0.0002, 0.0002}
	priceE := []float32{0.00005, 0.00005}

	out := AggregateOutcome(run, flows, priceN, priceE)

	assert.Empty(t, out.Notes, "no gaps expected")
	require.NotNil(t, out.ActualCost)
	// 500*0.0002 + 0*0.0002 - 0*0.00005 - 200*0.00005 = 0.1 - 0.01 = 0.09 EUR
	assert.InDelta(t, 0.09, *out.ActualCost, 1e-6)

	require.NotNil(t, out.ActualGridWh)
	assert.InDelta(t, 500, *out.ActualGridWh, 1e-9)
	require.NotNil(t, out.ActualFeedinWh)
	assert.InDelta(t, 200, *out.ActualFeedinWh, 1e-9)

	require.NotNil(t, out.ActualPvWh)
	assert.InDelta(t, 700, *out.ActualPvWh, 1e-9)

	require.NotNil(t, out.ActualBatteryChargeWh)
	assert.InDelta(t, 100, *out.ActualBatteryChargeWh, 1e-9)
	require.NotNil(t, out.ActualBatteryDischargeWh)
	assert.InDelta(t, 50, *out.ActualBatteryDischargeWh, 1e-9)

	require.NotNil(t, out.ActualHomeWh)
	assert.InDelta(t, 550, *out.ActualHomeWh, 1e-9)
	require.NotNil(t, out.ActualLoadpointWh)
	assert.InDelta(t, 1500, *out.ActualLoadpointWh, 1e-9)

	require.NotNil(t, out.ActualSelfConsumedWh)
	assert.InDelta(t, 500, *out.ActualSelfConsumedWh, 1e-9) // 700 pv - 200 feedin

	assert.Equal(t, ws, out.WindowStart)
	assert.Equal(t, ws.Add(2*tariff.SlotDuration), out.WindowEnd)
}

func TestAggregateOutcome_PartialGridSlots(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 4, SlotDurationS: 900}

	flows := HouseFlows{
		Grid: []SlotEnergy{
			{Start: ws, Energy: 0.5, ReturnEnergy: 0},
			{Start: ws.Add(tariff.SlotDuration), Energy: 0.5, ReturnEnergy: 0},
			// slots 2 and 3 missing
		},
	}
	out := AggregateOutcome(run, flows, flatPrices(4, 0.0002), flatPrices(4, 0.00005))

	require.NotNil(t, out.ActualGridWh)
	assert.InDelta(t, 1000, *out.ActualGridWh, 1e-9, "partial sum populated")
	assert.Contains(t, out.Notes, "partial grid: 2/4 slots")
}

func TestAggregateOutcome_MissingPrices_NotesOnlyNoNumerics(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 4, SlotDurationS: 900}

	out := AggregateOutcome(run, HouseFlows{
		Grid: []SlotEnergy{{Start: ws, Energy: 1.0}},
	}, flatPrices(2, 0.0002) /* short */, flatPrices(4, 0.00005))

	assert.Nil(t, out.ActualCost)
	assert.Nil(t, out.ActualGridWh)
	assert.Nil(t, out.ActualFeedinWh)
	assert.Contains(t, out.Notes, "priceN shorter than horizon")
}

func TestAggregateOutcome_NoPv_HomeBatteryStillRecorded(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 1, SlotDurationS: 900}

	flows := HouseFlows{
		Grid:    []SlotEnergy{{Start: ws, Energy: 0.5, ReturnEnergy: 0}},
		Home:    []SlotEnergy{{Start: ws, Energy: 0.4, ReturnEnergy: 0}},
		Battery: []SlotEnergy{{Start: ws, Energy: 0.1, ReturnEnergy: 0}},
	}
	out := AggregateOutcome(run, flows, flatPrices(1, 0.0002), flatPrices(1, 0.00005))

	require.NotNil(t, out.ActualCost)
	require.NotNil(t, out.ActualGridWh)
	require.NotNil(t, out.ActualHomeWh)
	require.NotNil(t, out.ActualBatteryChargeWh)
	assert.Nil(t, out.ActualPvWh, "pv missing → nil")
	assert.Nil(t, out.ActualSelfConsumedWh, "no pv → can't derive self-consumption")
	assert.Contains(t, out.Notes, "no pv data")
}

func TestAggregateOutcome_SelfConsumptionDerivation(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 1, SlotDurationS: 900}

	flows := HouseFlows{
		Grid: []SlotEnergy{{Start: ws, Energy: 0, ReturnEnergy: 2.0}}, // feedin 2 kWh
		PV:   []SlotEnergy{{Start: ws, Energy: 5.0, ReturnEnergy: 0}}, // produced 5 kWh
	}
	out := AggregateOutcome(run, flows, flatPrices(1, 0.0002), flatPrices(1, 0.00005))

	require.NotNil(t, out.ActualSelfConsumedWh)
	assert.InDelta(t, 3000, *out.ActualSelfConsumedWh, 1e-9) // 5000 - 2000
}

func TestAggregateOutcome_SelfConsumptionFloorAtZero(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 1, SlotDurationS: 900}

	// metering quirk: feedin > pv on a slot boundary
	flows := HouseFlows{
		Grid: []SlotEnergy{{Start: ws, Energy: 0, ReturnEnergy: 3.0}},
		PV:   []SlotEnergy{{Start: ws, Energy: 1.0, ReturnEnergy: 0}},
	}
	out := AggregateOutcome(run, flows, flatPrices(1, 0.0002), flatPrices(1, 0.00005))

	require.NotNil(t, out.ActualSelfConsumedWh)
	assert.Equal(t, 0.0, *out.ActualSelfConsumedWh, "must not go negative")
}

func TestAggregateOutcome_NonStandardSlotDur(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 4, SlotDurationS: 600} // 10-min slots

	out := AggregateOutcome(run, HouseFlows{
		Grid: []SlotEnergy{{Start: ws, Energy: 1.0}},
	}, flatPrices(4, 0.0002), flatPrices(4, 0.00005))

	assert.Nil(t, out.ActualCost)
	assert.Nil(t, out.ActualGridWh)
	assert.Contains(t, out.Notes, "unexpected slot duration: 600s")
}

func TestAggregateOutcome_FeedinReducesCost(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 1, SlotDurationS: 900}

	flows := HouseFlows{
		Grid: []SlotEnergy{{Start: ws, Energy: 1.0, ReturnEnergy: 2.0}}, // imp 1kWh, exp 2kWh
	}
	out := AggregateOutcome(run, flows, flatPrices(1, 0.0002), flatPrices(1, 0.00005))

	require.NotNil(t, out.ActualCost)
	// 1000*0.0002 - 2000*0.00005 = 0.2 - 0.1 = 0.1 EUR
	assert.InDelta(t, 0.1, *out.ActualCost, 1e-6)
	require.NotNil(t, out.ActualFeedinWh)
	assert.InDelta(t, 2000, *out.ActualFeedinWh, 1e-9)
}

func TestAggregateOutcome_LoadpointSummedAcross(t *testing.T) {
	setupAbLogDB(t)
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)

	insertSlot(t, Loadpoint, "lp-1", ws, 1.5, 0)
	insertSlot(t, Loadpoint, "lp-2", ws, 0.5, 0)

	slots, err := SlotsForGroup(Loadpoint, ws.Add(-time.Minute), ws.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, slots, 1)
	assert.InDelta(t, 2.0, slots[0].Energy, 1e-9)
}

func TestAggregateOutcome_EmptyAllGroups(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 4, SlotDurationS: 900}

	out := AggregateOutcome(run, HouseFlows{}, flatPrices(4, 0.0002), flatPrices(4, 0.00005))

	assert.Nil(t, out.ActualCost)
	assert.Nil(t, out.ActualGridWh)
	assert.Contains(t, out.Notes, "no meter data")
}

func TestAggregateOutcome_BatteryChargeOnly(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws, Horizon: 1, SlotDurationS: 900}

	flows := HouseFlows{
		Grid:    []SlotEnergy{{Start: ws, Energy: 0.5}},
		Battery: []SlotEnergy{{Start: ws, Energy: 0.3, ReturnEnergy: 0.0}}, // only charging
	}
	out := AggregateOutcome(run, flows, flatPrices(1, 0.0002), flatPrices(1, 0.00005))

	require.NotNil(t, out.ActualBatteryChargeWh)
	assert.InDelta(t, 300, *out.ActualBatteryChargeWh, 1e-9)
	require.NotNil(t, out.ActualBatteryDischargeWh)
	assert.InDelta(t, 0, *out.ActualBatteryDischargeWh, 1e-9)
}

// TestSlotsForGroup_FiltersOutRangeStart guards the site_ab_outcome.go
// contract: SlotsForGroup uses m.ts >= from, so callers MUST pass a truncated
// windowStart if they want to include the slot rooted at that boundary.
// Passing a mid-slot moment excludes the slot rooted at the previous
// boundary — this is the shape of the off-by-one that caused every early
// outcome to be N-1/N slots short.
func TestSlotsForGroup_FiltersOutRangeStart(t *testing.T) {
	setupAbLogDB(t)
	slot := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	insertSlot(t, Grid, "g", slot, 1.0, 0)

	// mid-slot windowStart → excludes the slot at 14:00
	midSlotStart := slot.Add(7 * time.Minute)
	got, err := SlotsForGroup(Grid, midSlotStart, slot.Add(2*tariff.SlotDuration))
	require.NoError(t, err)
	assert.Empty(t, got, "mid-slot from filters out the slot rooted at the earlier boundary")

	// truncated windowStart → includes it
	got, err = SlotsForGroup(Grid, slot, slot.Add(2*tariff.SlotDuration))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 1.0, got[0].Energy, 1e-9)
}

func TestAggregateOutcome_WindowEndComputed(t *testing.T) {
	ws := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC).Truncate(tariff.SlotDuration)
	run := AbRun{ID: 1, Timestamp: ws.Add(7 * time.Second) /* off-slot start */, Horizon: 4, SlotDurationS: 900}

	out := AggregateOutcome(run, HouseFlows{}, flatPrices(4, 0.0002), flatPrices(4, 0.00005))

	// Timestamp is truncated to the slot boundary before WindowEnd is computed,
	// so an off-slot start does NOT shift the window.
	assert.Equal(t, ws, out.WindowStart)
	assert.Equal(t, ws.Add(time.Hour), out.WindowEnd)
}
