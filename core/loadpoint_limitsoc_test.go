package core

import (
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/settings"
	serverdb "github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// socControllerCharger is a mock charger that also implements api.SocController,
// recording the values written to the device.
type socControllerCharger struct {
	*api.MockCharger
	limit  int64
	writes int
}

func (c *socControllerCharger) SetLimitSoc(limit int64) error {
	c.limit = limit
	c.writes++
	return nil
}

func TestSetLimitSocPushesToDevice(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	charger := &socControllerCharger{MockCharger: api.NewMockCharger(ctrl)}

	lp := &Loadpoint{
		log:      util.NewLogger("foo"),
		settings: settings.NewDatabaseSettingsAdapter("foo"),
		charger:  charger,
	}

	// a positive limit is written to the device
	lp.SetLimitSoc(50)
	assert.Equal(t, 50, lp.limitSoc)
	assert.Equal(t, int64(50), charger.limit)
	assert.Equal(t, 1, charger.writes)

	// changing the limit writes again
	lp.SetLimitSoc(55)
	assert.Equal(t, int64(55), charger.limit)
	assert.Equal(t, 2, charger.writes)

	// setting the same value is a no-op (no device write)
	lp.SetLimitSoc(55)
	assert.Equal(t, 2, charger.writes)

	// clearing the limit (0) must NOT be written to the device
	lp.SetLimitSoc(0)
	assert.Equal(t, 0, lp.limitSoc)
	assert.Equal(t, 2, charger.writes, "clearing the limit must not write to the device")
	assert.Equal(t, int64(55), charger.limit)
}

// TestSetLimitSocWithoutController ensures a plain charger (no api.SocController)
// is handled gracefully.
func TestSetLimitSocWithoutController(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lp := &Loadpoint{
		log:      util.NewLogger("foo"),
		settings: settings.NewDatabaseSettingsAdapter("foo"),
		charger:  api.NewMockCharger(ctrl),
	}

	lp.SetLimitSoc(50)
	assert.Equal(t, 50, lp.limitSoc)
}
