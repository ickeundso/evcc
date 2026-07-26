package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandlerPersistsImmediately verifies that the generic per-parameter handler
// flushes settings to the database right away, so loadpoint changes (e.g. smart
// cost limit) survive a restart without depending on the periodic flush ticker.
func TestHandlerPersistsImmediately(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, db.NewInstance("sqlite", dsn))

	h := handler(
		func(s string) (string, error) { return s, nil },
		func(v string) error { settings.SetString("lp1.smartCostLimit", v); return nil },
		func() string { s, _ := settings.String("lp1.smartCostLimit"); return s },
	)

	req := httptest.NewRequest(http.MethodPost, "/x/0.05", nil)
	req = mux.SetURLVars(req, map[string]string{"value": "0.05"})
	rec := httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// reopen from disk: the value must have persisted, not just live in memory
	require.NoError(t, db.NewInstance("sqlite", dsn))
	got, err := settings.String("lp1.smartCostLimit")
	require.NoError(t, err)
	assert.Equal(t, "0.05", got)
}
