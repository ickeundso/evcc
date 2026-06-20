package metrics

import (
	"fmt"
	"strings"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
)

// A/B outcome aggregator helpers.
//
// PendingRuns picks runs whose forecast window has fully elapsed and that
// still have no ab_actual_outcomes row. SlotsForGroup queries the collector
// meters table for one entity group in a time range. AggregateOutcome reduces
// the slot rows + per-slot prices from the persisted request into an AbOutcome.
//
// All functions here are pure or read-only; the only write path remains the
// existing PersistOutcome upsert.

// canonicalSlotSeconds is the standard 15-min slot length used by the
// collector. Runs with a different slot_dur_s are rejected by AggregateOutcome.
const canonicalSlotSeconds = 900

// SlotEnergy is one 15-min collector row. Both values are kWh - the unit the
// collector persists at db.go's persist().
//
// Interpretation depends on the entity group:
//
//	Grid:      Energy = import,     ReturnEnergy = export
//	PV:        Energy = production, ReturnEnergy = 0
//	Battery:   Energy = charging,   ReturnEnergy = discharging
//	Home:      Energy = consumed,   ReturnEnergy = 0
//	Loadpoint: Energy = ev_charge,  ReturnEnergy = 0
type SlotEnergy struct {
	Start        time.Time
	Energy       float64
	ReturnEnergy float64
}

// HouseFlows bundles per-group slot data for one outcome window. Each slice
// is already summed across all entities of that group (e.g. multi-PV / multi-
// battery / multi-loadpoint installations). A nil/empty slice means "no data
// for this group" - AggregateOutcome turns that into nil numerics + a note.
type HouseFlows struct {
	Grid      []SlotEnergy
	PV        []SlotEnergy
	Battery   []SlotEnergy
	Home      []SlotEnergy
	Loadpoint []SlotEnergy
}

// PendingRuns returns runs whose [ts, ts + horizon*slot_dur_s) window ended at
// or before cutoff and that have no ab_actual_outcomes row yet. Oldest first,
// capped at limit. Implemented with LEFT JOIN + IS NULL so the candidate set
// is computed in SQLite, not pulled into Go.
func PendingRuns(cutoff time.Time, limit int) ([]AbRun, error) {
	var runs []AbRun
	tx := db.Instance.
		Table("ab_optimizer_runs r").
		Select("r.*").
		Joins("LEFT JOIN ab_actual_outcomes o ON o.run_id = r.id").
		Where("o.run_id IS NULL").
		Where(
			"datetime(r.ts, '+' || (r.horizon * r.slot_dur_s) || ' seconds') <= ?",
			cutoff.UTC().Format("2006-01-02 15:04:05.999999999-07:00"),
		).
		Order("r.ts ASC").
		Limit(limit)
	if err := tx.Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// SlotsForGroup queries the meters table for one entity group in [from, to).
// Slots are returned in ascending time order. Multiple entities of the same
// group (multi-PV, multi-battery, multi-loadpoint) are summed per slot.
//
// Uses the post-0.309.1 column names energy / return_energy. Mirrors the
// SQL pattern in db_history.go's QueryEnergy.
func SlotsForGroup(group string, from, to time.Time) ([]SlotEnergy, error) {
	type row struct {
		StartUnix    int64
		Energy       float64
		ReturnEnergy float64
	}

	var rows []row
	err := db.Instance.
		Table("meters m").
		Select(`m.ts AS start_unix,
			COALESCE(SUM(m.energy), 0) AS energy,
			COALESCE(SUM(m.return_energy), 0) AS return_energy`).
		Joins("JOIN entities e ON m.meter = e.id").
		Where(`e."group" = ?`, group).
		Where("m.ts >= ? AND m.ts < ?", from.Unix(), to.Unix()).
		Group("m.ts").
		Order("m.ts").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]SlotEnergy, len(rows))
	for i, r := range rows {
		out[i] = SlotEnergy{
			Start:        time.Unix(r.StartUnix, 0).UTC(),
			Energy:       r.Energy,
			ReturnEnergy: r.ReturnEnergy,
		}
	}
	return out, nil
}

// AggregateOutcome reduces per-group slot rows + per-slot prices into an
// AbOutcome. Pure function - no DB writes.
//
// priceN and priceE are the grid-import / feed-in prices stored in the run's
// request (EUR/kWh per slot, already through scaleAndPrune *0.001 so prices
// are EUR/Wh - we multiply Wh*EUR/Wh below).
//
// Cost formula matches MILP's grid-only objective:
//
//	cost = Σ ( grid_import_kWh * priceN - grid_export_kWh * priceE )
//
// Self-consumption: self_consumed_pv_Wh = max(0, pv_Wh - feedin_Wh).
//
// Inputs may be missing or partial. The function never panics; it returns
// an AbOutcome with nil numerics for absent groups and a non-empty Notes
// listing every gap. Cases:
//
//   - slot_dur_s != 900            → notes-only, all numerics nil
//   - len(priceN) < horizon        → notes-only, all numerics nil
//   - len(priceE) < horizon        → notes-only, all numerics nil
//   - every group empty            → notes-only, all numerics nil
//   - flows.X partial or empty     → numeric for X reflects what's there,
//     notes describes the gap
func AggregateOutcome(
	run AbRun,
	flows HouseFlows,
	priceN, priceE []float32,
) AbOutcome {
	out := AbOutcome{RunID: run.ID}

	windowStart := run.Timestamp.Truncate(tariff.SlotDuration)
	windowEnd := windowStart.Add(time.Duration(run.Horizon*run.SlotDurationS) * time.Second)
	out.WindowStart = windowStart
	out.WindowEnd = windowEnd

	var notes []string

	if run.SlotDurationS != canonicalSlotSeconds {
		notes = append(notes, fmt.Sprintf("unexpected slot duration: %ds", run.SlotDurationS))
		out.Notes = strings.Join(notes, "; ")
		return out
	}
	if len(priceN) < run.Horizon {
		notes = append(notes, fmt.Sprintf("priceN shorter than horizon: %d<%d", len(priceN), run.Horizon))
	}
	if len(priceE) < run.Horizon {
		notes = append(notes, fmt.Sprintf("priceE shorter than horizon: %d<%d", len(priceE), run.Horizon))
	}
	if len(notes) > 0 {
		out.Notes = strings.Join(notes, "; ")
		return out
	}

	if len(flows.Grid) == 0 && len(flows.PV) == 0 && len(flows.Battery) == 0 &&
		len(flows.Home) == 0 && len(flows.Loadpoint) == 0 {
		out.Notes = "no meter data for window"
		return out
	}

	// Index each per-group slot list by Unix timestamp so the cost computation
	// can pick the correct price[i] without depending on per-group sparsity.
	gridIdx := indexSlots(flows.Grid)

	// Cost is grid-only: self-consumed PV is "free", which is exactly why we
	// optimize for it. Sum over slots that actually have grid data.
	var costSum, gridImpWh, feedinWh float64
	var gridCovered int
	for i := range run.Horizon {
		slotStart := windowStart.Add(time.Duration(i) * time.Duration(run.SlotDurationS) * time.Second).Unix()
		s, ok := gridIdx[slotStart]
		if !ok {
			continue
		}
		// collector stores kWh per slot; convert to Wh for the *_wh columns
		impWh := s.Energy * 1000
		expWh := s.ReturnEnergy * 1000
		gridImpWh += impWh
		feedinWh += expWh
		// cost: kWh * EUR/kWh — priceN/priceE are EUR/kWh in the request scale
		// (verified: typical p_N ≈ 0.0002 in raw, but already *0.001'd → that
		// makes it EUR/Wh; here we want EUR so kWh*EUR/kWh OR Wh*EUR/Wh).
		// Convention: priceN/priceE are stored as EUR/Wh after scaleAndPrune,
		// so multiply Wh by them.
		costSum += impWh*float64(priceN[i]) - expWh*float64(priceE[i])
		gridCovered++
	}

	if gridCovered == 0 {
		notes = append(notes, "no grid data for window")
	} else {
		v := costSum
		out.ActualCost = &v
		gi := gridImpWh
		out.ActualGridWh = &gi
		fi := feedinWh
		out.ActualFeedinWh = &fi
		if gridCovered < run.Horizon {
			notes = append(notes, fmt.Sprintf("partial grid: %d/%d slots", gridCovered, run.Horizon))
		}
	}

	pvWh, pvCovered := sumWindow(flows.PV, windowStart, run)
	if pvCovered == 0 {
		notes = append(notes, "no pv data")
	} else {
		v := pvWh
		out.ActualPvWh = &v
		if pvCovered < run.Horizon {
			notes = append(notes, fmt.Sprintf("partial pv: %d/%d slots", pvCovered, run.Horizon))
		}
	}

	batChWh, batChCov, batDisWh, batDisCov := sumWindowDual(flows.Battery, windowStart, run)
	if batChCov == 0 && batDisCov == 0 {
		notes = append(notes, "no battery data")
	} else {
		bc := batChWh
		out.ActualBatteryChargeWh = &bc
		bd := batDisWh
		out.ActualBatteryDischargeWh = &bd
		cov := max(batChCov, batDisCov)
		if cov < run.Horizon {
			notes = append(notes, fmt.Sprintf("partial battery: %d/%d slots", cov, run.Horizon))
		}
	}

	homeWh, homeCovered := sumWindow(flows.Home, windowStart, run)
	if homeCovered == 0 {
		notes = append(notes, "no home data")
	} else {
		v := homeWh
		out.ActualHomeWh = &v
		if homeCovered < run.Horizon {
			notes = append(notes, fmt.Sprintf("partial home: %d/%d slots", homeCovered, run.Horizon))
		}
	}

	lpWh, lpCovered := sumWindow(flows.Loadpoint, windowStart, run)
	if lpCovered == 0 {
		notes = append(notes, "no loadpoint data")
	} else {
		v := lpWh
		out.ActualLoadpointWh = &v
		if lpCovered < run.Horizon {
			notes = append(notes, fmt.Sprintf("partial loadpoint: %d/%d slots", lpCovered, run.Horizon))
		}
	}

	// Self-consumption derives from grid + pv. Floor at zero - metering
	// quirks can push feedin > pv momentarily on slot boundaries.
	if out.ActualPvWh != nil && out.ActualFeedinWh != nil {
		sc := *out.ActualPvWh - *out.ActualFeedinWh
		if sc < 0 {
			sc = 0
		}
		out.ActualSelfConsumedWh = &sc
	}

	out.Notes = strings.Join(notes, "; ")
	return out
}

// indexSlots keys a SlotEnergy slice by Start.Unix() for quick lookup by slot.
func indexSlots(slots []SlotEnergy) map[int64]SlotEnergy {
	out := make(map[int64]SlotEnergy, len(slots))
	for _, s := range slots {
		out[s.Start.Unix()] = s
	}
	return out
}

// sumWindow sums the Energy field across slots that fall inside the run's
// window. Returns Wh (collector persists kWh) and the count of slots seen.
func sumWindow(slots []SlotEnergy, windowStart time.Time, run AbRun) (float64, int) {
	idx := indexSlots(slots)
	var total float64
	var covered int
	for i := range run.Horizon {
		slotStart := windowStart.Add(time.Duration(i) * time.Duration(run.SlotDurationS) * time.Second).Unix()
		if s, ok := idx[slotStart]; ok {
			total += s.Energy * 1000
			covered++
		}
	}
	return total, covered
}

// sumWindowDual sums both Energy (charge) and ReturnEnergy (discharge) for
// battery flows. Returns chargeWh, chargeCount, dischargeWh, dischargeCount.
func sumWindowDual(slots []SlotEnergy, windowStart time.Time, run AbRun) (float64, int, float64, int) {
	idx := indexSlots(slots)
	var ch, dis float64
	var chCov, disCov int
	for i := range run.Horizon {
		slotStart := windowStart.Add(time.Duration(i) * time.Duration(run.SlotDurationS) * time.Second).Unix()
		s, ok := idx[slotStart]
		if !ok {
			continue
		}
		ch += s.Energy * 1000
		dis += s.ReturnEnergy * 1000
		chCov++
		disCov++
	}
	return ch, chCov, dis, disCov
}
