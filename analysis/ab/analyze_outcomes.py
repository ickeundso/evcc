"""Planned-vs-actual analysis using ab_actual_outcomes rows.

Complements analyze.py (which characterizes solver-vs-solver on request/response
alone) by joining in the actuals collected by the outcome aggregator. Answers:

1. Are outcomes healthy? (Notes distribution, coverage of the 5 groups)
2. How accurate is the *forecast* (planned vs actual PV, home, loadpoint)?
3. Given the plan the solver produced, how close was actual grid cost to the
   grid cost implied by the plan (response-derived, NOT objective_value)?
4. What does self-consumption reality look like on the 10-day full-house window?

Key finding baked into the code: MILP's `objective_value` includes battery
terminal value (PA term) and cycling penalties. It is NOT the grid cost.
Predicted grid cost must be derived from the response arrays.

Usage:
    python3 analysis/ab/analyze_outcomes.py [path-to-export.db]
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

sys.path.insert(0, str(Path(__file__).parent))
import loader  # noqa: E402

CHARTS = Path(__file__).parent / "charts"
CHARTS.mkdir(exist_ok=True)

COLOR_MILP = "#1f77b4"
COLOR_ML = "#ff7f0e"
COLOR_ACTUAL = "#2ca02c"


def _save(fig, name):
    path = CHARTS / name
    fig.tight_layout()
    fig.savefig(path, dpi=120, bbox_inches="tight")
    plt.close(fig)
    return path


def reasonable_mask(df: pd.DataFrame) -> pd.Series:
    """Drop outcomes whose values are physically impossible for a residential
    setup. Some historical meter rows contain 64 GWh-scale garbage. Caps:
    grid/feedin < 500 kWh, pv/home/loadpoint/battery < 200 kWh, cost < 100 €.
    """
    def cap(s, m):
        return (s.isna()) | (s.abs() < m)

    return (
        cap(df.actual_grid_wh, 500_000)
        & cap(df.actual_feedin_wh, 500_000)
        & cap(df.actual_cost, 100)
        & cap(df.actual_pv_wh, 200_000)
        & cap(df.actual_home_wh, 200_000)
        & cap(df.actual_loadpoint_wh, 200_000)
        & cap(df.actual_battery_charge_wh, 200_000)
        & cap(df.actual_battery_discharge_wh, 200_000)
    )


def section_o1_outcome_health(outcomes: pd.DataFrame) -> dict:
    """Coverage of each group over the whole outcomes table."""
    n = len(outcomes)
    coverage = {
        "grid": outcomes.actual_cost.notna().mean(),
        "pv": outcomes.actual_pv_wh.notna().mean(),
        "battery": outcomes.actual_battery_charge_wh.notna().mean(),
        "home": outcomes.actual_home_wh.notna().mean(),
        "loadpoint": outcomes.actual_loadpoint_wh.notna().mean(),
    }
    reasonable = reasonable_mask(outcomes.dropna(subset=["actual_cost"]))
    out_ok = outcomes.dropna(subset=["actual_cost"]).loc[reasonable]

    # Coverage over time (by day)
    o = outcomes.copy()
    o["day"] = o.window_start.dt.tz_convert("Europe/Berlin").dt.date
    cov_daily = (
        o.groupby("day")[
            ["actual_pv_wh", "actual_battery_charge_wh", "actual_loadpoint_wh"]
        ]
        .apply(lambda g: g.notna().mean())
    )

    fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(12, 6))
    ax1.bar(coverage.keys(), [v * 100 for v in coverage.values()], color=[COLOR_MILP, COLOR_ML, "#9467bd", "#e377c2", "#8c564b"])
    ax1.set_ylabel("% of outcomes populated")
    ax1.set_title("Section O1 — outcome coverage per group (all-time)")
    ax1.grid(axis="y", alpha=0.3)

    for col, color in [
        ("actual_pv_wh", COLOR_ML),
        ("actual_battery_charge_wh", "#9467bd"),
        ("actual_loadpoint_wh", "#8c564b"),
    ]:
        ax2.plot(cov_daily.index, cov_daily[col] * 100, label=col.replace("actual_", "").replace("_wh", ""), color=color)
    ax2.set_ylabel("daily coverage %")
    ax2.set_xlabel("date")
    ax2.set_title("coverage rises when 0.309.1-use-ml.2 lands (~2026-06-23)")
    ax2.legend()
    ax2.grid(alpha=0.3)
    chart = _save(fig, "o1_outcome_health.png")

    return {
        "chart": str(chart),
        "total_outcomes": int(n),
        "coverage_pct": {k: round(v * 100, 1) for k, v in coverage.items()},
        "outliers_dropped": int(len(outcomes.dropna(subset=["actual_cost"])) - reasonable.sum()),
        "reasonable_rows": int(reasonable.sum()),
        "full_house_rows": int((outcomes.actual_pv_wh.notna() & outcomes.actual_battery_charge_wh.notna()).sum()),
    }


def compute_response_grid_cost(response: dict, price_n: list, price_e: list) -> tuple[float, float, float]:
    """Return (grid_cost_eur, grid_import_wh, grid_export_wh) DERIVED from the
    response's per-slot arrays. This is the correct predicted-cost quantity —
    NOT objective_value, which includes battery terminal value + cycling terms.
    """
    if not response:
        return (np.nan, np.nan, np.nan)
    gi = response.get("grid_import") or []
    ge = response.get("grid_export") or []
    n = min(len(gi), len(ge), len(price_n), len(price_e))
    cost = sum(gi[i] * price_n[i] - ge[i] * price_e[i] for i in range(n))
    return cost, sum(gi[:n]), sum(ge[:n])


def section_o2_forecast_quality(runs: pd.DataFrame, outcomes: pd.DataFrame) -> dict:
    """PV forecast vs actual PV for runs where we have both.

    ft (from request) is Wh per slot. Sum across horizon = expected total PV
    over the window. Compare against actual_pv_wh.
    """
    merged = runs.merge(outcomes[["run_id", "actual_pv_wh", "actual_home_wh"]], on="run_id", how="inner")
    merged = merged[merged.actual_pv_wh.notna()]

    rows = []
    for r in merged.itertuples():
        req = r.request
        if not req:
            continue
        ts = req.get("time_series", {})
        ft = ts.get("ft") or []
        gt = ts.get("gt") or []
        rows.append(
            {
                "run_id": r.run_id,
                "ts": r.ts,
                "horizon": r.horizon,
                "pv_forecast_wh": sum(ft),
                "pv_actual_wh": r.actual_pv_wh,
                "home_forecast_wh": sum(gt),
                "home_actual_wh": r.actual_home_wh,
            }
        )
    df = pd.DataFrame(rows)
    df["pv_error_wh"] = df.pv_forecast_wh - df.pv_actual_wh
    df["pv_rel_error"] = df.pv_error_wh / df.pv_actual_wh.clip(lower=100)
    df["home_error_wh"] = df.home_forecast_wh - df.home_actual_wh

    # Cap for plotting sanity — some historical rows still have bogus actuals
    keep = (df.pv_actual_wh > 0) & (df.pv_actual_wh < 500_000) & (df.pv_forecast_wh < 500_000)
    p = df[keep]

    fig, axes = plt.subplots(1, 3, figsize=(15, 4.5))

    ax = axes[0]
    ax.scatter(p.pv_actual_wh / 1000, p.pv_forecast_wh / 1000, s=3, alpha=0.3, color=COLOR_ML)
    hi = float(max(p.pv_actual_wh.max(), p.pv_forecast_wh.max()) / 1000)
    ax.plot([0, hi], [0, hi], "--", color="grey", linewidth=1, label="identity")
    ax.set_xlabel("actual PV (kWh over horizon)")
    ax.set_ylabel("forecast PV (kWh)")
    ax.set_title("Section O2 — PV forecast vs actual")
    ax.legend()
    ax.grid(alpha=0.3)

    ax = axes[1]
    ax.hist(p.pv_error_wh / 1000, bins=80, color=COLOR_ML, alpha=0.7)
    ax.set_xlabel("PV error (kWh, forecast − actual)")
    ax.set_ylabel("# runs")
    ax.set_title(f"forecast bias (mean {p.pv_error_wh.mean()/1000:+.1f} kWh)")
    ax.axvline(0, color="grey", linewidth=1)
    ax.grid(alpha=0.3)

    ax = axes[2]
    kh = (df.home_forecast_wh > 0) & (df.home_actual_wh > 0) & (df.home_actual_wh < 200_000)
    hp = df[kh]
    ax.scatter(hp.home_actual_wh / 1000, hp.home_forecast_wh / 1000, s=3, alpha=0.3, color=COLOR_MILP)
    hi = float(max(hp.home_actual_wh.max(), hp.home_forecast_wh.max()) / 1000)
    ax.plot([0, hi], [0, hi], "--", color="grey", linewidth=1)
    ax.set_xlabel("actual home (kWh)")
    ax.set_ylabel("forecast home (kWh)")
    ax.set_title("home forecast vs actual")
    ax.grid(alpha=0.3)

    chart = _save(fig, "o2_forecast_quality.png")

    # Robust stats: median + p90 abs error, skip the mean-of-ratios.
    return {
        "chart": str(chart),
        "samples": int(len(p)),
        "pv_median_error_kwh": round(float(p.pv_error_wh.median() / 1000), 3),
        "pv_median_abs_error_kwh": round(float(p.pv_error_wh.abs().median() / 1000), 3),
        "pv_p90_abs_error_kwh": round(float(p.pv_error_wh.abs().quantile(0.9) / 1000), 3),
        "home_median_error_wh": round(float((df[kh].home_forecast_wh - df[kh].home_actual_wh).median()), 1),
        "home_median_abs_error_wh": round(float((df[kh].home_forecast_wh - df[kh].home_actual_wh).abs().median()), 1),
    }


def section_o3_plan_cost_vs_actual(paired: pd.DataFrame, db_path: str) -> dict:
    """Derive predicted grid cost from response arrays and compare to actual.

    Samples 1500 runs from the full-house subset. Filters outliers.
    """
    p = paired[paired.actual_pv_wh.notna()]
    p = p[reasonable_mask(p)]
    sample = p.sample(min(1500, len(p)), random_state=42) if len(p) > 1500 else p

    rows = []
    for r in sample.itertuples():
        req = loader.load_request_for_run(r.run_id, db_path)
        ts = req.get("time_series", {})
        price_n = ts.get("p_N") or []
        price_e = ts.get("p_E") or []
        responses = loader.load_responses_for_run(r.run_id, db_path)
        milp = responses.get("milp")
        ml = responses.get("ml")

        milp_cost, milp_gi, milp_ge = compute_response_grid_cost(milp, price_n, price_e)
        ml_cost, ml_gi, ml_ge = compute_response_grid_cost(ml, price_n, price_e)

        rows.append(
            {
                "run_id": r.run_id,
                "ts": r.ts,
                "horizon": r.horizon,
                "actual_cost": r.actual_cost,
                "milp_predicted_grid_cost": milp_cost,
                "ml_predicted_grid_cost": ml_cost,
                "milp_predicted_grid_import_wh": milp_gi,
                "milp_predicted_grid_export_wh": milp_ge,
                "actual_grid_wh": r.actual_grid_wh,
                "actual_feedin_wh": r.actual_feedin_wh,
                "milp_objective_reported": r.milp_predicted_cost,
            }
        )
    df = pd.DataFrame(rows)
    df["milp_err"] = df.milp_predicted_grid_cost - df.actual_cost
    df["ml_err"] = df.ml_predicted_grid_cost - df.actual_cost
    df["obj_vs_derived_gap"] = df.milp_objective_reported - df.milp_predicted_grid_cost

    fig, axes = plt.subplots(1, 3, figsize=(15, 4.5))

    ax = axes[0]
    ax.scatter(df.actual_cost, df.milp_predicted_grid_cost, s=6, alpha=0.4, label="MILP", color=COLOR_MILP)
    ax.scatter(df.actual_cost, df.ml_predicted_grid_cost, s=6, alpha=0.4, label="ML", color=COLOR_ML)
    lo = float(min(df.actual_cost.min(), df.milp_predicted_grid_cost.min()))
    hi = float(max(df.actual_cost.max(), df.milp_predicted_grid_cost.max()))
    ax.plot([lo, hi], [lo, hi], "--", color="grey", linewidth=1, label="identity")
    ax.set_xlabel("actual grid cost (€)")
    ax.set_ylabel("response-derived predicted grid cost (€)")
    ax.set_title("Section O3 — predicted vs actual grid cost")
    ax.legend()
    ax.grid(alpha=0.3)

    ax = axes[1]
    ax.hist(df.milp_err.dropna(), bins=60, color=COLOR_MILP, alpha=0.6, label="MILP err")
    ax.hist(df.ml_err.dropna(), bins=60, color=COLOR_ML, alpha=0.4, label="ML err")
    ax.set_xlabel("predicted − actual (€)")
    ax.set_ylabel("# runs")
    ax.axvline(0, color="grey", linewidth=1)
    ax.set_title(f"error histograms (MAE MILP {df.milp_err.abs().mean():.2f}, ML {df.ml_err.abs().mean():.2f})")
    ax.legend()
    ax.grid(alpha=0.3)

    ax = axes[2]
    ax.scatter(df.milp_predicted_grid_cost, df.milp_objective_reported, s=6, alpha=0.4, color=COLOR_MILP)
    lo = float(min(df.milp_predicted_grid_cost.min(), df.milp_objective_reported.min()))
    hi = float(max(df.milp_predicted_grid_cost.max(), df.milp_objective_reported.max()))
    ax.plot([lo, hi], [lo, hi], "--", color="grey", linewidth=1)
    ax.set_xlabel("grid cost derived from response arrays (€)")
    ax.set_ylabel("MILP objective_value (€)")
    ax.set_title(f"objective ≠ grid cost (mean gap {df.obj_vs_derived_gap.mean():.2f} €)")
    ax.grid(alpha=0.3)

    chart = _save(fig, "o3_plan_cost_vs_actual.png")

    return {
        "chart": str(chart),
        "sample_size": int(len(df)),
        "milp_derived_mae_eur": round(float(df.milp_err.abs().mean()), 3),
        "ml_derived_mae_eur": round(float(df.ml_err.abs().mean()), 3),
        "milp_derived_bias_eur": round(float(df.milp_err.mean()), 3),
        "objective_minus_derived_bias_eur": round(float(df.obj_vs_derived_gap.mean()), 3),
    }


def section_o4_whole_house_reality(outcomes: pd.DataFrame) -> dict:
    """Actual per-pathway means on the full-house window (June 23 onward)."""
    full = outcomes[
        outcomes.actual_pv_wh.notna()
        & outcomes.actual_battery_charge_wh.notna()
        & outcomes.actual_loadpoint_wh.notna()
        & outcomes.actual_home_wh.notna()
    ]
    full = full[reasonable_mask(full)]

    # Medians are robust to the residual outliers that reasonable_mask misses.
    metrics = {
        "grid_import": float(full.actual_grid_wh.median()),
        "grid_export": float(full.actual_feedin_wh.median()),
        "pv_production": float(full.actual_pv_wh.median()),
        "self_consumed_pv": float(full.actual_self_consumed_wh.median()),
        "battery_charge": float(full.actual_battery_charge_wh.median()),
        "battery_discharge": float(full.actual_battery_discharge_wh.median()),
        "home_consumption": float(full.actual_home_wh.median()),
        "loadpoint_charging": float(full.actual_loadpoint_wh.median()),
    }

    fig, ax = plt.subplots(figsize=(11, 5))
    names = list(metrics.keys())
    values = [metrics[k] for k in names]
    ax.bar(names, values, color=COLOR_ACTUAL, alpha=0.8)
    ax.set_xticklabels([n.replace("_", "\n") for n in names], rotation=0, fontsize=9)
    ax.set_ylabel("median Wh per run window (actual, robust to outliers)")
    ax.set_title(f"Section O4 — actual whole-house flows (n={len(full)}, ~10 days of full-house data)")
    ax.grid(axis="y", alpha=0.3)
    chart = _save(fig, "o4_whole_house_reality.png")

    # Derived KPIs
    autarky = 1 - metrics["grid_import"] / (metrics["home_consumption"] + metrics["loadpoint_charging"])
    self_cons_ratio = metrics["self_consumed_pv"] / metrics["pv_production"] if metrics["pv_production"] > 0 else 0

    return {
        "chart": str(chart),
        "sample_size": int(len(full)),
        "means_wh": {k: round(v, 1) for k, v in metrics.items()},
        "autarky_pct": round(autarky * 100, 1),
        "self_consumption_pct": round(self_cons_ratio * 100, 1),
    }


def main(db_path: str | Path) -> dict:
    db_path = str(db_path)
    print(f"Loading {db_path}...")

    runs = loader.load_runs(db_path)
    outcomes = loader.load_outcomes(db_path)
    paired = loader.load_paired_with_outcomes(db_path)

    out = {
        "input_db": db_path,
        "runs_total": int(len(runs)),
        "outcomes_total": int(len(outcomes)),
        "paired_with_outcomes": int(len(paired)),
    }
    print("Section O1...");  out["o1"] = section_o1_outcome_health(outcomes)
    print("Section O2 (slow: parses request JSON)...");  out["o2"] = section_o2_forecast_quality(runs, outcomes)
    print("Section O3 (slow: samples 1500 runs, reads response JSON)...");  out["o3"] = section_o3_plan_cost_vs_actual(paired, db_path)
    print("Section O4...");  out["o4"] = section_o4_whole_house_reality(outcomes)

    result_path = Path(__file__).parent / "analyze_outcomes_results.json"
    result_path.write_text(json.dumps(out, indent=2, default=str))
    print(f"\nWrote summary to {result_path}")
    return out


if __name__ == "__main__":
    db = sys.argv[1] if len(sys.argv) > 1 else str(loader.DEFAULT_DB)
    main(db)
