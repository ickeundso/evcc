package metrics

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupAbLogDB creates a fresh in-memory SQLite instance with the A/B log
// tables migrated. The package-level db.Instance is replaced for the duration
// of the test.
func setupAbLogDB(t *testing.T) {
	t.Helper()
	require.NoError(t, db.NewInstance("sqlite", ":memory:"))
}

func TestAbLog_PersistRun_ReturnsID(t *testing.T) {
	setupAbLogDB(t)

	ts := time.Date(2026, 4, 8, 14, 0, 0, 0, time.UTC)
	id, err := PersistRun(ts, 48, 15*time.Minute, `{"test":"payload"}`)
	require.NoError(t, err)
	assert.Greater(t, id, uint(0))

	// second run gets a different id
	id2, err := PersistRun(ts.Add(time.Minute), 48, 15*time.Minute, "")
	require.NoError(t, err)
	assert.Greater(t, id2, id)
}

func TestAbLog_PersistRun_EmptyRequest(t *testing.T) {
	setupAbLogDB(t)

	id, err := PersistRun(time.Now(), 0, 0, "")
	require.NoError(t, err)
	assert.Greater(t, id, uint(0))
}

func TestAbLog_PersistResponse_Success(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	objVal := 12.34
	require.NoError(t, PersistResponse(
		runID,
		"milp",
		time.Now(),
		250*time.Millisecond,
		"Optimal",
		"",
		&objVal,
		`{"status":"Optimal"}`,
	))

	responses, err := QueryResponses(runID)
	require.NoError(t, err)
	require.Len(t, responses, 1)

	got := responses[0]
	assert.Equal(t, runID, got.RunID)
	assert.Equal(t, "milp", got.Source)
	assert.Equal(t, "Optimal", got.Status)
	assert.Equal(t, int64(250), got.DurationMs)
	assert.Empty(t, got.Error)
	require.NotNil(t, got.ObjectiveValue)
	assert.InDelta(t, 12.34, *got.ObjectiveValue, 1e-9)
	assert.Equal(t, `{"status":"Optimal"}`, got.Response)
}

func TestAbLog_PersistResponse_Error(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	require.NoError(t, PersistResponse(
		runID,
		"ml",
		time.Now(),
		100*time.Millisecond,
		"error",
		"connection refused",
		nil,
		"",
	))

	responses, err := QueryResponses(runID)
	require.NoError(t, err)
	require.Len(t, responses, 1)

	got := responses[0]
	assert.Equal(t, "error", got.Status)
	assert.Equal(t, "connection refused", got.Error)
	assert.Nil(t, got.ObjectiveValue)
	assert.Empty(t, got.Response)
}

func TestAbLog_PersistResponse_RejectsZeroRunID(t *testing.T) {
	setupAbLogDB(t)

	err := PersistResponse(0, "milp", time.Now(), 0, "Optimal", "", nil, "")
	assert.ErrorIs(t, err, ErrNoRunID)
}

func TestAbLog_PersistResponse_UniqueRunSource(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	require.NoError(t, PersistResponse(runID, "milp", time.Now(), 0, "Optimal", "", nil, ""))

	// same (run_id, source) must fail
	err = PersistResponse(runID, "milp", time.Now(), 0, "Optimal", "", nil, "")
	assert.Error(t, err, "duplicate (run_id, source) should be rejected")

	// different source for the same run is fine
	require.NoError(t, PersistResponse(runID, "ml", time.Now(), 0, "Optimal", "", nil, ""))

	responses, err := QueryResponses(runID)
	require.NoError(t, err)
	assert.Len(t, responses, 2)
}

func TestAbLog_PersistOutcome_Insert(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	cost := 1.23
	gridWh := 2500.0
	pvWh := 500.0
	feedinWh := 100.0

	require.NoError(t, PersistOutcome(AbOutcome{
		RunID:          runID,
		WindowStart:    time.Date(2026, 4, 8, 14, 0, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 4, 8, 15, 0, 0, 0, time.UTC),
		ActualCost:     &cost,
		ActualGridWh:   &gridWh,
		ActualPvWh:     &pvWh,
		ActualFeedinWh: &feedinWh,
	}))

	got, err := QueryOutcome(runID)
	require.NoError(t, err)
	assert.Equal(t, runID, got.RunID)
	require.NotNil(t, got.ActualCost)
	assert.InDelta(t, 1.23, *got.ActualCost, 1e-9)
	require.NotNil(t, got.ActualGridWh)
	assert.InDelta(t, 2500.0, *got.ActualGridWh, 1e-9)
}

func TestAbLog_PersistOutcome_Upsert(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	cost1 := 1.0
	require.NoError(t, PersistOutcome(AbOutcome{
		RunID:      runID,
		ActualCost: &cost1,
		Notes:      "initial",
	}))

	cost2 := 2.5
	require.NoError(t, PersistOutcome(AbOutcome{
		RunID:      runID,
		ActualCost: &cost2,
		Notes:      "corrected",
	}))

	got, err := QueryOutcome(runID)
	require.NoError(t, err)
	require.NotNil(t, got.ActualCost)
	assert.InDelta(t, 2.5, *got.ActualCost, 1e-9)
	assert.Equal(t, "corrected", got.Notes)
}

func TestAbLog_PersistOutcome_RejectsZeroRunID(t *testing.T) {
	setupAbLogDB(t)

	err := PersistOutcome(AbOutcome{})
	assert.ErrorIs(t, err, ErrNoRunID)
}

func TestAbLog_QueryOutcome_NotFound(t *testing.T) {
	setupAbLogDB(t)

	_, err := QueryOutcome(42)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestAbLog_QueryRuns_TimeRange(t *testing.T) {
	setupAbLogDB(t)

	base := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)

	for i := range 5 {
		_, err := PersistRun(base.Add(time.Duration(i)*time.Hour), 48, 15*time.Minute, "")
		require.NoError(t, err)
	}

	// [13:00, 15:00) must return the runs at 13:00 and 14:00
	got, err := QueryRuns(base.Add(time.Hour), base.Add(3*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.True(t, got[0].Timestamp.Before(got[1].Timestamp), "results must be sorted ascending")
}

func TestAbLog_QueryResponses_Empty(t *testing.T) {
	setupAbLogDB(t)

	runID, err := PersistRun(time.Now(), 48, 15*time.Minute, `{}`)
	require.NoError(t, err)

	got, err := QueryResponses(runID)
	require.NoError(t, err)
	assert.Empty(t, got)
}
