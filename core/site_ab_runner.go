package core

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/sponsor"
	optimizer "github.com/evcc-io/optimizer/client"
)

// A/B optimizer runner — wires the shadow evaluation harness from
// site_ab_optimizer.go into the site update loop. Separate rate limiter
// and mutex from the MILP optimizer so both paths run independently.

var (
	abUpdated time.Time
	abMu      atomic.Uint32
)

// abOptimizerUpdateAsync is the goroutine-safe entry point called from
// site.update(). It mirrors the shape of optimizerUpdateAsync: rate-limited
// to at most once per 2 minutes, mutex-guarded against concurrent runs,
// with panic recovery.
func (site *Site) abOptimizerUpdateAsync() {
	if time.Since(abUpdated) < 2*time.Minute {
		return
	}

	if !abMu.CompareAndSwap(0, 1) {
		return
	}

	defer func() {
		abUpdated = time.Now()
		abMu.Store(0)

		if r := recover(); r != nil {
			site.log.ERROR.Printf("ab optimizer: panic %v", r)
		}
	}()

	if err := site.abOptimizerUpdate(); err != nil {
		site.log.ERROR.Printf("ab optimizer: %v", err)
	}
}

// abOptimizerUpdate builds the shared optimizer request and fans it out to
// all configured optimizer backends for shadow comparison. Results are
// persisted to the ab_optimizer_* SQLite tables by runAbOptimizers.
func (site *Site) abOptimizerUpdate() error {
	mlURI := os.Getenv("ML_OPTIMIZER_URI")
	if mlURI == "" {
		return nil
	}

	req, _, err := site.buildOptimizerRequest(site.battery.Devices)
	if err != nil {
		return err
	}

	// Always include the ML backend (user's own service).
	backends := []AbOptimizerBackend{
		{Source: AbOptimizerSourceML, URI: mlURI},
	}

	// Include the MILP backend for side-by-side comparison when available.
	if milpURI := os.Getenv("OPTIMIZER_URI"); milpURI != "" {
		// Prepend so MILP results appear first in persisted rows.
		backends = append([]AbOptimizerBackend{
			{Source: AbOptimizerSourceMILP, URI: milpURI},
		}, backends...)
	}

	httpClient := request.NewClient(site.log)
	httpClient.Timeout = 30 * time.Second

	authFn := func(_ context.Context, req *http.Request) error {
		if sponsor.IsAuthorized() {
			req.Header.Set("Authorization", "Bearer "+sponsor.Token)
		}
		return nil
	}

	runID, results, err := runAbOptimizers(
		context.Background(), site.log, httpClient,
		backends, req, authFn, nil,
	)
	if err != nil {
		return err
	}

	for _, r := range results {
		if r.Err != nil {
			site.log.DEBUG.Printf("ab optimizer: run %d %s error: %v", runID, r.Source, r.Err)
		} else {
			site.log.DEBUG.Printf("ab optimizer: run %d %s %s (objective=%.4f, %s)",
				runID, r.Source, r.Status,
				func() float64 {
					if r.ObjectiveValue != nil {
						return *r.ObjectiveValue
					}
					return 0
				}(),
				r.Duration)
		}
	}

	return nil
}

// sponsorAuthFn returns an optimizer.RequestEditorFn that attaches the
// sponsor bearer token when available.
func sponsorAuthFn() optimizer.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		if sponsor.IsAuthorized() {
			req.Header.Set("Authorization", "Bearer "+sponsor.Token)
		}
		return nil
	}
}
