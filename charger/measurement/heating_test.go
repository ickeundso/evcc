package measurement

import (
	"testing"

	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemperatureConfigKeys guards the mapstructure keys used by templates.
// util.DecodeOther has ErrorUnused: true, so a template key that does not match
// a struct field (as happened with "settemp" vs "setlimittemp") fails at
// instantiation. skiptest templates don't catch this, so pin it here.
func TestTemperatureConfigKeys(t *testing.T) {
	var tc Temperature
	err := util.DecodeOther(map[string]any{
		"temp":         map[string]any{"source": "const", "value": 1},
		"limittemp":    map[string]any{"source": "const", "value": 1},
		"setlimittemp": map[string]any{"source": "const", "value": 1},
	}, &tc)
	require.NoError(t, err)
	assert.NotNil(t, tc.Temp)
	assert.NotNil(t, tc.LimitTemp)
	assert.NotNil(t, tc.SetLimitTemp)
}

func TestTemperatureRejectsUnknownKey(t *testing.T) {
	var tc Temperature
	err := util.DecodeOther(map[string]any{
		"settemp": map[string]any{"source": "const"},
	}, &tc)
	require.Error(t, err)
}
