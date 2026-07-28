"""Baseline PV-forecast calibration for the evcc A/B export.

Question: can a simple model beat the built-in solar forecast at the run-window
level, i.e. predicting actual_pv_wh over the ~17 h horizon?

- Baseline    = the built-in forecast, sum(ft).
- Bias-corr   = forecast + median(train residual)  (cheapest possible fix).
- Model       = HistGradientBoosting on [forecast, home_forecast, prices,
                hour/day-of-year cyclic]  (handles NaN natively).

Strict TIME split (train = earlier period, test = later) so overlapping 17 h
windows can't leak across the boundary. One row per unique window (freshest run)
to avoid oversampling the same window ~7x.

    python3 analysis/ab/pv_forecast.py [export.db]
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
from sklearn.ensemble import HistGradientBoostingRegressor
from sklearn.inspection import permutation_importance
from sklearn.metrics import mean_absolute_error, r2_score

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import loader  # noqa: E402

FEATURES = [
    "pv_forecast_wh",
    "home_forecast_wh",
    "price_import_mean",
    "price_export_mean",
    "n_slots_fc",
    "hour_sin",
    "hour_cos",
    "doy_sin",
    "doy_cos",
]


def main(db: str) -> None:
    df = loader.load_run_features(db)
    if df.empty:
        raise SystemExit("no rows with actual PV outcomes in export")

    # keep runs with a real PV actual + a sane forecast
    df = df[
        df.actual_pv_wh.notna()
        & (df.actual_pv_wh > 0)
        & (df.actual_pv_wh < 200_000)
        & (df.pv_forecast_wh >= 0)
        & (df.pv_forecast_wh < 200_000)
    ].copy()

    # one row per 15-min window (freshest forecast) to reduce overlap oversampling
    df = df.sort_values("ts").drop_duplicates("window_start", keep="last")
    df = df.sort_values("window_start").reset_index(drop=True)

    n = len(df)
    if n < 200:
        raise SystemExit(f"only {n} usable windows — too few to model")

    cut = int(n * 0.7)
    train, test = df.iloc[:cut], df.iloc[cut:]

    Xtr, ytr = train[FEATURES], train["actual_pv_wh"].to_numpy()
    Xte, yte = test[FEATURES], test["actual_pv_wh"].to_numpy()

    # baseline: raw forecast
    base_pred = test["pv_forecast_wh"].to_numpy()
    base_mae = mean_absolute_error(yte, base_pred)

    # cheapest fix: constant bias correction learned on train
    bias = float(np.median(ytr - train["pv_forecast_wh"].to_numpy()))
    biascorr_mae = mean_absolute_error(yte, base_pred + bias)

    # model
    model = HistGradientBoostingRegressor(
        max_iter=500,
        learning_rate=0.04,
        l2_regularization=1.0,
        min_samples_leaf=40,
        random_state=0,
    )
    model.fit(Xtr, ytr)
    pred = model.predict(Xte)
    mdl_mae = mean_absolute_error(yte, pred)

    imp = permutation_importance(model, Xte, yte, n_repeats=5, random_state=0, scoring="neg_mean_absolute_error")
    importance = {
        f: round(float(v) / 1000, 3)  # kWh MAE contribution
        for f, v in sorted(zip(FEATURES, imp.importances_mean), key=lambda x: -x[1])
    }

    result = {
        "db": str(db),
        "windows_used": int(n),
        "train_rows": int(len(train)),
        "test_rows": int(len(test)),
        "train_span": [str(train.window_start.min()), str(train.window_start.max())],
        "test_span": [str(test.window_start.min()), str(test.window_start.max())],
        "baseline_forecast_mae_kwh": round(base_mae / 1000, 3),
        "bias_corrected_mae_kwh": round(biascorr_mae / 1000, 3),
        "model_mae_kwh": round(mdl_mae / 1000, 3),
        "model_vs_forecast_improvement_pct": round(100 * (base_mae - mdl_mae) / base_mae, 1),
        "model_r2": round(r2_score(yte, pred), 3),
        "forecast_r2": round(r2_score(yte, base_pred), 3),
        "train_median_bias_kwh": round(bias / 1000, 3),
        "permutation_importance_kwh": importance,
    }
    print(json.dumps(result, indent=2))
    (HERE / "pv_forecast_results.json").write_text(json.dumps(result, indent=2))

    # charts
    fig, axes = plt.subplots(1, 2, figsize=(12, 4.8))
    ax = axes[0]
    hi = float(max(yte.max(), base_pred.max()) / 1000)
    ax.plot([0, hi], [0, hi], "--", color="grey", lw=1, label="identity")
    ax.scatter(yte / 1000, base_pred / 1000, s=4, alpha=0.25, label=f"forecast (MAE {base_mae/1000:.2f})")
    ax.scatter(yte / 1000, pred / 1000, s=4, alpha=0.25, color="tab:green", label=f"model (MAE {mdl_mae/1000:.2f})")
    ax.set_xlabel("actual PV (kWh over horizon)")
    ax.set_ylabel("predicted PV (kWh)")
    ax.set_title("PV forecast vs model — test period")
    ax.legend(fontsize=8)
    ax.grid(alpha=0.3)

    ax = axes[1]
    ax.hist((base_pred - yte) / 1000, bins=70, alpha=0.5, label="forecast error")
    ax.hist((pred - yte) / 1000, bins=70, alpha=0.5, color="tab:green", label="model error")
    ax.axvline(0, color="grey", lw=1)
    ax.set_xlabel("error (kWh, predicted − actual)")
    ax.set_ylabel("# windows")
    ax.set_title("error distribution — test period")
    ax.legend(fontsize=8)
    ax.grid(alpha=0.3)

    fig.tight_layout()
    out = HERE / "charts" / "pv_forecast.png"
    out.parent.mkdir(exist_ok=True)
    fig.savefig(out, dpi=110)
    print(f"\nWrote chart to {out}")


if __name__ == "__main__":
    db = sys.argv[1] if len(sys.argv) > 1 else str(loader.DEFAULT_DB)
    main(db)
