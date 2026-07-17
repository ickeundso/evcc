package measurement

import (
	"context"
	"fmt"

	"github.com/evcc-io/evcc/plugin"
)

type Temperature struct {
	Temp         *plugin.Config // optional
	LimitTemp    *plugin.Config // optional, read device limit/target temperature
	SetLimitTemp *plugin.Config // optional, write device limit/target temperature
}

func (cc *Temperature) Configure(ctx context.Context) (
	func() (float64, error),
	func() (int64, error),
	func(int64) error,
	error,
) {
	tempG, err := cc.Temp.FloatGetter(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("temp: %w", err)
	}

	limitTempG, err := cc.LimitTemp.IntGetter(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("limit temp: %w", err)
	}

	limitTempS, err := cc.SetLimitTemp.IntSetter(ctx, "limittemp")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("set limit temp: %w", err)
	}

	return tempG, limitTempG, limitTempS, nil
}
