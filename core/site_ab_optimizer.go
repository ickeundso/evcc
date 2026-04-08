package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/util"
	optimizer "github.com/evcc-io/optimizer/client"
)

// A/B shadow evaluation for optimizer backends.
//
// This file wires parallel optimizer calls (typically the existing
// evcc-io/optimizer MILP backend and an evcc-ml-optimizer ML backend) against
// the same request, persists every call via the metrics.AbLog* APIs and
// returns the individual results to the caller.
//
// Phase 1 is pure shadow mode: nothing in here feeds into loadpoint or battery
// control decisions. The orchestrator is intentionally self-contained and has
// no dependency on *Site so it can be unit-tested in isolation.

// AbOptimizerSource identifies an optimizer backend in persisted rows.
type AbOptimizerSource string

// Known optimizer backend identifiers.
const (
	AbOptimizerSourceMILP AbOptimizerSource = "milp"
	AbOptimizerSourceML   AbOptimizerSource = "ml"
)

// AbOptimizerBackend describes a single optimizer endpoint to query.
type AbOptimizerBackend struct {
	Source AbOptimizerSource
	URI    string
}

// AbOptimizerResult captures the outcome of one backend call. Err is
// non-nil for transport / decoding / non-Optimal responses, but the run is
// still persisted so that failed calls remain visible in the comparison.
type AbOptimizerResult struct {
	Source         AbOptimizerSource
	URI            string
	Duration       time.Duration
	Status         string
	Err            error
	ObjectiveValue *float64
	Response       *optimizer.OptimizationResult
}

// AbOptimizerClientFactory constructs an optimizer client for a given URI.
// Injected into runAbOptimizers so that tests can substitute a fake transport
// without touching the httptest servers directly.
type AbOptimizerClientFactory func(uri string, httpClient *http.Client) (optimizer.ClientWithResponsesInterface, error)

// defaultAbOptimizerClientFactory is the production factory wrapping the
// generated optimizer client.
func defaultAbOptimizerClientFactory(uri string, httpClient *http.Client) (optimizer.ClientWithResponsesInterface, error) {
	return optimizer.NewClientWithResponses(uri, optimizer.WithHTTPClient(httpClient))
}

// runAbOptimizers calls every configured backend in parallel with the same
// request, persists the run and each response, and returns the individual
// results. The returned runID is zero only when no run could be persisted
// (typically a database failure before any call was made).
//
// The function returns an error only for unrecoverable setup failures
// (nil/empty input, persistence of the run header failing). Individual
// backend failures are captured in the per-result Err field so that a partial
// A/B (one backend up, one down) is still a useful data point.
//
// authFn is applied to every outgoing request and may be nil. It is typically
// used to attach the sponsor bearer token.
func runAbOptimizers(
	ctx context.Context,
	log *util.Logger,
	httpClient *http.Client,
	backends []AbOptimizerBackend,
	req *optimizer.OptimizationInput,
	authFn optimizer.RequestEditorFn,
	clientFactory AbOptimizerClientFactory,
) (uint, []AbOptimizerResult, error) {
	if req == nil {
		return 0, nil, errors.New("ab optimizer: nil request")
	}
	if len(backends) == 0 {
		return 0, nil, errors.New("ab optimizer: no backends configured")
	}
	if clientFactory == nil {
		clientFactory = defaultAbOptimizerClientFactory
	}

	requestJSON, err := json.Marshal(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ab optimizer: marshal request: %w", err)
	}

	horizon, slotDur := inferHorizonAndSlotDuration(req)

	runID, err := metrics.PersistRun(time.Now(), horizon, slotDur, string(requestJSON))
	if err != nil {
		return 0, nil, fmt.Errorf("ab optimizer: persist run: %w", err)
	}

	log.DEBUG.Printf("ab optimizer: run %d started with %d backends (horizon=%d, slot=%s)",
		runID, len(backends), horizon, slotDur)

	results := make([]AbOptimizerResult, len(backends))
	var wg sync.WaitGroup
	for i, backend := range backends {
		wg.Add(1)
		go func(idx int, backend AbOptimizerBackend) {
			defer wg.Done()

			// Per-backend panic recovery: a broken backend must not take down
			// the other calls or the caller goroutine.
			defer func() {
				if r := recover(); r != nil {
					results[idx].Err = fmt.Errorf("panic: %v", r)
					results[idx].Status = "error"
					log.ERROR.Printf("ab optimizer: %s panic: %v", backend.Source, r)
					persistResponseLogError(log, runID, backend, results[idx])
				}
			}()

			result := callAbBackend(ctx, backend, httpClient, req, authFn, clientFactory)
			results[idx] = result

			if result.Err != nil {
				log.DEBUG.Printf("ab optimizer: %s failed after %s: %v",
					backend.Source, result.Duration, result.Err)
			} else {
				log.DEBUG.Printf("ab optimizer: %s %s after %s (objective=%v)",
					backend.Source, result.Status, result.Duration, result.ObjectiveValue)
			}

			persistResponseLogError(log, runID, backend, result)
		}(i, backend)
	}

	wg.Wait()

	return runID, results, nil
}

// callAbBackend performs a single optimizer call and maps the response into an
// AbOptimizerResult. It never returns an error: all failure modes are
// translated into result.Err so that the caller gets a uniform per-backend
// picture.
func callAbBackend(
	ctx context.Context,
	backend AbOptimizerBackend,
	httpClient *http.Client,
	req *optimizer.OptimizationInput,
	authFn optimizer.RequestEditorFn,
	clientFactory AbOptimizerClientFactory,
) AbOptimizerResult {
	result := AbOptimizerResult{
		Source: backend.Source,
		URI:    backend.URI,
	}

	client, err := clientFactory(backend.URI, httpClient)
	if err != nil {
		result.Err = fmt.Errorf("create client: %w", err)
		result.Status = "error"
		return result
	}

	editors := make([]optimizer.RequestEditorFn, 0, 1)
	if authFn != nil {
		editors = append(editors, authFn)
	}

	start := time.Now()
	resp, err := client.PostOptimizeChargeScheduleWithResponse(ctx, *req, editors...)
	result.Duration = time.Since(start)

	if err != nil {
		result.Err = err
		result.Status = "error"
		return result
	}

	if resp.StatusCode() != http.StatusOK {
		result.Err = fmt.Errorf("http %d", resp.StatusCode())
		result.Status = "error"
		return result
	}

	if resp.JSON200 == nil {
		result.Err = errors.New("empty response body")
		result.Status = "error"
		return result
	}

	result.Response = resp.JSON200
	result.Status = string(resp.JSON200.Status)

	if resp.JSON200.Status == optimizer.Optimal {
		objVal := float64(resp.JSON200.ObjectiveValue)
		result.ObjectiveValue = &objVal
	} else {
		// Non-Optimal statuses (Infeasible, Unbounded, ...) are not transport
		// errors, but we surface them as Err so that callers can tell at a
		// glance which responses are actionable.
		result.Err = fmt.Errorf("solver status: %s", resp.JSON200.Status)
	}

	return result
}

// persistResponseLogError writes the result to the A/B log. Persistence
// failures are logged but not surfaced - the caller already has the in-memory
// result, and losing one row must not cascade.
func persistResponseLogError(
	log *util.Logger,
	runID uint,
	backend AbOptimizerBackend,
	result AbOptimizerResult,
) {
	status := result.Status
	if status == "" {
		if result.Err != nil {
			status = "error"
		} else {
			status = string(optimizer.Undefined)
		}
	}

	errMsg := ""
	if result.Err != nil {
		errMsg = result.Err.Error()
	}

	responseJSON := ""
	if result.Response != nil {
		if b, err := json.Marshal(result.Response); err == nil {
			responseJSON = string(b)
		} else {
			log.ERROR.Printf("ab optimizer: marshal %s response: %v", backend.Source, err)
		}
	}

	if err := metrics.PersistResponse(
		runID,
		string(backend.Source),
		time.Now(),
		result.Duration,
		status,
		errMsg,
		result.ObjectiveValue,
		responseJSON,
	); err != nil {
		log.ERROR.Printf("ab optimizer: persist %s response: %v", backend.Source, err)
	}
}

// inferHorizonAndSlotDuration derives the request horizon (number of slots)
// and the canonical slot duration (mode of dt[]) for logging / later analysis.
// Empty or malformed time series default to zero without erroring - the
// request payload is still persisted in full.
func inferHorizonAndSlotDuration(req *optimizer.OptimizationInput) (int, time.Duration) {
	dt := req.TimeSeries.Dt
	if len(dt) == 0 {
		return 0, 0
	}

	// Prefer the last slot's duration: the first slot is routinely a partial
	// slot (remaining time in the current 15-min window) which would give a
	// misleading "canonical" value.
	canonical := dt[len(dt)-1]
	return len(dt), time.Duration(canonical) * time.Second
}
