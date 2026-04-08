package metrics

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A/B optimizer shadow evaluation tables.
//
// These tables capture parallel optimizer runs (MILP vs. ML) with the same
// input request to enable empirical comparison of solver outputs against the
// actual system behaviour in the same time window. Phase 1 is pure shadow
// mode - nothing in here influences loadpoint or battery control decisions.

// AbRun is a single A/B trigger event. The request is shared between all
// optimizer backends that are called for this run.
type AbRun struct {
	ID        uint      `gorm:"column:id;primarykey"`
	Timestamp time.Time `gorm:"column:ts;index:idx_ab_runs_ts"`
	// Horizon is the number of time slots covered by the request.
	Horizon int `gorm:"column:horizon"`
	// SlotDurationS is the canonical slot duration in seconds (typically 900).
	SlotDurationS int `gorm:"column:slot_dur_s"`
	// Request is the JSON-serialised optimizer.OptimizationInput.
	Request string `gorm:"column:request"`
}

// TableName implements gorm.Tabler.
func (AbRun) TableName() string { return "ab_optimizer_runs" }

// AbResponse is one optimizer response linked to an AbRun. The (run_id, source)
// pair is unique - calling the same backend twice for the same run is a
// programming error.
type AbResponse struct {
	ID         uint      `gorm:"column:id;primarykey"`
	RunID      uint      `gorm:"column:run_id;uniqueIndex:idx_ab_resp_run_source;index:idx_ab_resp_run"`
	Source     string    `gorm:"column:source;uniqueIndex:idx_ab_resp_run_source"`
	Timestamp  time.Time `gorm:"column:ts"`
	DurationMs int64     `gorm:"column:duration_ms"`
	// Status mirrors optimizer.OptimizationResultStatus ("Optimal", "Infeasible"
	// etc.) on success or "error" when Error is non-empty.
	Status string `gorm:"column:status"`
	// Error holds a one-line error message when the backend call failed.
	// Empty string on success.
	Error string `gorm:"column:error"`
	// ObjectiveValue is the solver's objective function value (monetary benefit)
	// when available. NULL for failed/non-optimal calls.
	ObjectiveValue *float64 `gorm:"column:objective_value"`
	// Response is the JSON-serialised optimizer.OptimizationResult. Empty on
	// error.
	Response string `gorm:"column:response"`
}

// TableName implements gorm.Tabler.
func (AbResponse) TableName() string { return "ab_optimizer_responses" }

// AbOutcome is the actual system behaviour observed in the time window of an
// A/B run, filled in later by the outcome aggregator once enough meter data has
// accumulated. Keyed by RunID (1:1 with AbRun).
type AbOutcome struct {
	RunID          uint      `gorm:"column:run_id;primarykey"`
	WindowStart    time.Time `gorm:"column:window_start"`
	WindowEnd      time.Time `gorm:"column:window_end"`
	ActualCost     *float64  `gorm:"column:actual_cost"`
	ActualGridWh   *float64  `gorm:"column:actual_grid_wh"`
	ActualPvWh     *float64  `gorm:"column:actual_pv_wh"`
	ActualFeedinWh *float64  `gorm:"column:actual_feedin_wh"`
	Notes          string    `gorm:"column:notes"`
}

// TableName implements gorm.Tabler.
func (AbOutcome) TableName() string { return "ab_actual_outcomes" }

// ErrNoRunID signals that a persistence call received an invalid run id.
var ErrNoRunID = errors.New("ab_log: run id must be > 0")

func init() {
	db.Register(func(db *gorm.DB) error {
		return db.AutoMigrate(new(AbRun), new(AbResponse), new(AbOutcome))
	})
}

// PersistRun stores the shared request of an A/B run and returns the run id
// that all subsequent responses must reference. requestJSON is stored verbatim
// and may be empty.
func PersistRun(ts time.Time, horizon int, slotDur time.Duration, requestJSON string) (uint, error) {
	run := AbRun{
		Timestamp:     ts,
		Horizon:       horizon,
		SlotDurationS: int(slotDur.Seconds()),
		Request:       requestJSON,
	}

	if err := db.Instance.Create(&run).Error; err != nil {
		return 0, err
	}

	return run.ID, nil
}

// PersistResponse stores a single optimizer backend response for an existing
// run. Pass an empty responseJSON and a non-empty errMsg on failed calls. The
// (runID, source) pair is unique - duplicate inserts return an error.
func PersistResponse(
	runID uint,
	source string,
	ts time.Time,
	duration time.Duration,
	status string,
	errMsg string,
	objectiveValue *float64,
	responseJSON string,
) error {
	if runID == 0 {
		return ErrNoRunID
	}

	resp := AbResponse{
		RunID:          runID,
		Source:         source,
		Timestamp:      ts,
		DurationMs:     duration.Milliseconds(),
		Status:         status,
		Error:          errMsg,
		ObjectiveValue: objectiveValue,
		Response:       responseJSON,
	}

	return db.Instance.Create(&resp).Error
}

// PersistOutcome upserts the actual-outcome row for a run. Called by the
// outcome aggregator once the observation window has passed and meter data is
// available.
func PersistOutcome(outcome AbOutcome) error {
	if outcome.RunID == 0 {
		return ErrNoRunID
	}

	return db.Instance.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "run_id"}},
		UpdateAll: true,
	}).Create(&outcome).Error
}

// QueryRuns returns all A/B runs with a timestamp in [from, to), sorted by
// timestamp ascending.
func QueryRuns(from, to time.Time) ([]AbRun, error) {
	var runs []AbRun
	err := db.Instance.
		Where("ts >= ? AND ts < ?", from, to).
		Order("ts ASC").
		Find(&runs).Error
	return runs, err
}

// QueryResponses returns all responses for a given run, sorted by source.
func QueryResponses(runID uint) ([]AbResponse, error) {
	var responses []AbResponse
	err := db.Instance.
		Where("run_id = ?", runID).
		Order("source ASC").
		Find(&responses).Error
	return responses, err
}

// QueryOutcome returns the actual outcome for a run, or a zero-value outcome
// with gorm.ErrRecordNotFound if none has been recorded yet.
func QueryOutcome(runID uint) (AbOutcome, error) {
	var outcome AbOutcome
	err := db.Instance.
		Where("run_id = ?", runID).
		First(&outcome).Error
	return outcome, err
}
