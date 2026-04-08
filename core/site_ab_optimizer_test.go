package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/util"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAbOptimizerTest creates a fresh in-memory database and returns a logger
// and a sample request suitable for every orchestrator test.
//
// The orchestrator persists responses from multiple goroutines. With a plain
// ":memory:" DSN each new sqlite connection opened by the GORM pool would see
// its own private in-memory database without the migrated tables, which would
// lead to sporadic "no such table" errors depending on concurrency. Pinning
// the pool to a single connection matches the sequential behaviour that
// production (file-backed sqlite) exhibits and is the standard workaround
// for in-memory sqlite tests.
func setupAbOptimizerTest(t *testing.T) (*util.Logger, *optimizer.OptimizationInput) {
	t.Helper()
	require.NoError(t, db.NewInstance("sqlite", ":memory:"))
	sqlDB, err := db.Instance.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	log := util.NewLogger("test-ab-opt")
	req := sampleOptimizationInput()
	return log, req
}

// sampleOptimizationInput builds a minimal valid OptimizationInput for tests.
// Lengths are kept small (4 slots) so that the JSON payload is readable in
// assertions.
func sampleOptimizationInput() *optimizer.OptimizationInput {
	return &optimizer.OptimizationInput{
		EtaC: 0.9,
		EtaD: 0.9,
		TimeSeries: optimizer.TimeSeries{
			Dt: []int{600, 900, 900, 900},
			Gt: []float32{1000, 1000, 1000, 1000},
			Ft: []float32{500, 500, 500, 500},
			PN: []float32{0.30, 0.25, 0.20, 0.22},
			PE: []float32{0.10, 0.09, 0.08, 0.07},
		},
		Batteries: []optimizer.BatteryConfig{},
	}
}

// fakeOptimizerResponse builds a plausible optimizer response for a given
// status and objective value.
func fakeOptimizerResponse(status optimizer.OptimizationResultStatus, objVal float32) optimizer.OptimizationResult {
	return optimizer.OptimizationResult{
		Status:         status,
		ObjectiveValue: objVal,
		GridImport:     []float32{100, 200, 300, 400},
		GridExport:     []float32{0, 0, 0, 0},
		FlowDirection:  []optimizer.OptimizationResultFlowDirection{0, 0, 0, 0},
		Batteries:      []optimizer.BatteryResult{},
	}
}

// newFakeOptimizerServer starts a test server that replies to
// POST /optimize/charge-schedule with the given handler. The handler receives
// the decoded request and returns the response to send. Returning a nil body
// with status != 200 simulates an error response.
func newFakeOptimizerServer(t *testing.T, handler func(req optimizer.OptimizationInput) (int, any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/optimize/charge-schedule", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		defer r.Body.Close()

		var req optimizer.OptimizationInput
		require.NoError(t, json.Unmarshal(body, &req))

		status, resp := handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if resp != nil {
			require.NoError(t, json.NewEncoder(w).Encode(resp))
		}
	}))
}

func TestRunAbOptimizers_NilRequest(t *testing.T) {
	log, _ := setupAbOptimizerTest(t)

	runID, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		[]AbOptimizerBackend{{Source: AbOptimizerSourceMILP, URI: "http://x"}},
		nil, nil, nil,
	)

	assert.Error(t, err)
	assert.Zero(t, runID)
	assert.Nil(t, results)
}

func TestRunAbOptimizers_NoBackends(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	runID, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		nil, req, nil, nil,
	)

	assert.Error(t, err)
	assert.Zero(t, runID)
	assert.Nil(t, results)
}

func TestRunAbOptimizers_SuccessBoth(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	milpServer := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusOK, fakeOptimizerResponse(optimizer.Optimal, 1.23)
	})
	defer milpServer.Close()

	mlServer := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusOK, fakeOptimizerResponse(optimizer.Optimal, 1.50)
	})
	defer mlServer.Close()

	backends := []AbOptimizerBackend{
		{Source: AbOptimizerSourceMILP, URI: milpServer.URL},
		{Source: AbOptimizerSourceML, URI: mlServer.URL},
	}

	runID, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		backends, req, nil, nil,
	)
	require.NoError(t, err)
	require.NotZero(t, runID)
	require.Len(t, results, 2)

	// Backend results should be non-errored and have objective values.
	for _, r := range results {
		assert.NoError(t, r.Err, "%s should succeed", r.Source)
		assert.Equal(t, "Optimal", r.Status)
		require.NotNil(t, r.ObjectiveValue, "%s objective must be set", r.Source)
		assert.Positive(t, r.Duration)
	}

	// Sanity-check that both backends are actually distinguished.
	assert.InDelta(t, 1.23, *results[0].ObjectiveValue, 1e-6)
	assert.InDelta(t, 1.50, *results[1].ObjectiveValue, 1e-6)

	// And that persistence happened: 1 run, 2 responses.
	persistedResponses, err := metrics.QueryResponses(runID)
	require.NoError(t, err)
	require.Len(t, persistedResponses, 2)

	// Responses come back sorted by source alphabetically: milp, ml.
	assert.Equal(t, "milp", persistedResponses[0].Source)
	assert.Equal(t, "Optimal", persistedResponses[0].Status)
	assert.Empty(t, persistedResponses[0].Error)
	require.NotNil(t, persistedResponses[0].ObjectiveValue)
	assert.InDelta(t, 1.23, *persistedResponses[0].ObjectiveValue, 1e-6)
	assert.NotEmpty(t, persistedResponses[0].Response, "response JSON must be stored")

	assert.Equal(t, "ml", persistedResponses[1].Source)
	require.NotNil(t, persistedResponses[1].ObjectiveValue)
	assert.InDelta(t, 1.50, *persistedResponses[1].ObjectiveValue, 1e-6)
}

func TestRunAbOptimizers_OneBackendFails(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	okServer := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusOK, fakeOptimizerResponse(optimizer.Optimal, 2.0)
	})
	defer okServer.Close()

	failServer := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusInternalServerError, map[string]string{"message": "solver crashed"}
	})
	defer failServer.Close()

	backends := []AbOptimizerBackend{
		{Source: AbOptimizerSourceMILP, URI: okServer.URL},
		{Source: AbOptimizerSourceML, URI: failServer.URL},
	}

	runID, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		backends, req, nil, nil,
	)
	require.NoError(t, err)
	require.NotZero(t, runID)
	require.Len(t, results, 2)

	// MILP succeeds
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "Optimal", results[0].Status)
	require.NotNil(t, results[0].ObjectiveValue)

	// ML fails with HTTP 500
	require.Error(t, results[1].Err)
	assert.Contains(t, results[1].Err.Error(), "http 500")
	assert.Equal(t, "error", results[1].Status)
	assert.Nil(t, results[1].ObjectiveValue)

	// Both are persisted so we can still analyse the failure.
	persistedResponses, err := metrics.QueryResponses(runID)
	require.NoError(t, err)
	require.Len(t, persistedResponses, 2)

	// Find the error row (persisted order is milp, ml alphabetically).
	mlRow := persistedResponses[1]
	assert.Equal(t, "ml", mlRow.Source)
	assert.Equal(t, "error", mlRow.Status)
	assert.Contains(t, mlRow.Error, "http 500")
	assert.Nil(t, mlRow.ObjectiveValue)
}

func TestRunAbOptimizers_BothFail(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	// Both return transport errors via unreachable addresses.
	backends := []AbOptimizerBackend{
		{Source: AbOptimizerSourceMILP, URI: "http://127.0.0.1:1"},
		{Source: AbOptimizerSourceML, URI: "http://127.0.0.1:2"},
	}

	// Short per-call timeout so the test completes quickly.
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}

	runID, results, err := runAbOptimizers(
		t.Context(), log, httpClient,
		backends, req, nil, nil,
	)
	require.NoError(t, err, "setup errors should not bubble up from unreachable backends")
	require.NotZero(t, runID)
	require.Len(t, results, 2)

	for _, r := range results {
		assert.Error(t, r.Err)
		assert.Equal(t, "error", r.Status)
		assert.Nil(t, r.ObjectiveValue)
	}

	// Persistence still records both failures.
	persistedResponses, err := metrics.QueryResponses(runID)
	require.NoError(t, err)
	require.Len(t, persistedResponses, 2)
}

func TestRunAbOptimizers_NonOptimalStatus(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	infeasible := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusOK, fakeOptimizerResponse(optimizer.Infeasible, 0)
	})
	defer infeasible.Close()

	backends := []AbOptimizerBackend{
		{Source: AbOptimizerSourceMILP, URI: infeasible.URL},
	}

	runID, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		backends, req, nil, nil,
	)
	require.NoError(t, err)
	require.NotZero(t, runID)
	require.Len(t, results, 1)

	// Non-Optimal responses are persisted but flagged as errors so callers
	// can ignore them cleanly.
	result := results[0]
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "Infeasible")
	assert.Equal(t, "Infeasible", result.Status)
	assert.Nil(t, result.ObjectiveValue)
	assert.NotNil(t, result.Response, "response payload is still captured")
}

func TestRunAbOptimizers_AuthHeaderApplied(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	var receivedAuth string
	server := newFakeOptimizerServer(t, func(_ optimizer.OptimizationInput) (int, any) {
		return http.StatusOK, fakeOptimizerResponse(optimizer.Optimal, 1)
	})
	// Wrap the handler to capture the auth header before the body is read.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	defer server.Close()

	authFn := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer test-token")
		return nil
	}

	_, results, err := runAbOptimizers(
		t.Context(), log, http.DefaultClient,
		[]AbOptimizerBackend{{Source: AbOptimizerSourceMILP, URI: ts.URL}},
		req, authFn, nil,
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, "Bearer test-token", receivedAuth)
}

func TestRunAbOptimizers_ContextCancel(t *testing.T) {
	log, req := setupAbOptimizerTest(t)

	// Handler blocks until the server is closed so we can guarantee that the
	// context cancellation is the reason the call returns.
	blocker := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocker
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(blocker)
		server.Close()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	runID, results, err := runAbOptimizers(
		ctx, log, http.DefaultClient,
		[]AbOptimizerBackend{{Source: AbOptimizerSourceMILP, URI: server.URL}},
		req, nil, nil,
	)
	require.NoError(t, err)
	require.NotZero(t, runID)
	require.Len(t, results, 1)
	assert.Error(t, results[0].Err)
	assert.Equal(t, "error", results[0].Status)
}

func TestInferHorizonAndSlotDuration(t *testing.T) {
	tc := []struct {
		name        string
		dt          []int
		wantHorizon int
		wantSlot    time.Duration
	}{
		{"empty", nil, 0, 0},
		{"single slot", []int{900}, 1, 15 * time.Minute},
		{"partial first slot then canonical", []int{600, 900, 900, 900}, 4, 15 * time.Minute},
		{"hourly", []int{1200, 3600, 3600}, 3, time.Hour},
	}

	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			req := &optimizer.OptimizationInput{
				TimeSeries: optimizer.TimeSeries{Dt: tt.dt},
			}
			h, d := inferHorizonAndSlotDuration(req)
			assert.Equal(t, tt.wantHorizon, h)
			assert.Equal(t, tt.wantSlot, d)
		})
	}
}
