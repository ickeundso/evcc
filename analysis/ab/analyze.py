"""Run the full A/B export analysis.

Produces all charts under analysis/ab/charts/ and a draft `report.md`. Each
section is a standalone function so individual sections can be re-run from a
REPL.

Usage:
    python3 analysis/ab/analyze.py [path-to-export.db]

Default DB path: evcc-ab-export.db at repo root (see loader.DEFAULT_DB).

Originally planned as notebook.ipynb but kept as a script for simpler review +
diff-ability. Use jupytext to convert later if needed.
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

# Use a consistent palette throughout
COLOR_MILP = "#1f77b4"
COLOR_ML = "#ff7f0e"


def _save(fig: plt.Figure, name: str) -> Path:
    path = CHARTS / name
    fig.tight_layout()
    fig.savefig(path, dpi=120, bbox_inches="tight")
    plt.close(fig)
    return path


def section_1_dataset_health(runs: pd.DataFrame, resp: pd.DataFrame) -> dict:
    """Daily run count, gaps, error rate per day."""
    runs = runs.copy()
    runs["date"] = runs["ts"].dt.tz_convert("Europe/Berlin").dt.date
    daily_runs = runs.groupby("date").size()

    resp = resp.copy()
    resp["date"] = resp["ts"].dt.tz_convert("Europe/Berlin").dt.date
    errs_by_day = (
        resp[resp["status"] == "error"]
        .groupby(["date", "source"])
        .size()
        .unstack(fill_value=0)
    )
    totals_by_day = resp.groupby(["date", "source"]).size().unstack(fill_value=0)
    err_rate = (errs_by_day / totals_by_day).fillna(0)

    fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(12, 6), sharex=True)
    ax1.plot(daily_runs.index, daily_runs.values, color="black")
    ax1.set_ylabel("runs / day")
    ax1.set_title("Section 1 — dataset health")
    ax1.grid(alpha=0.3)

    for src, color in [("milp", COLOR_MILP), ("ml", COLOR_ML)]:
        if src in err_rate.columns:
            ax2.plot(err_rate.index, err_rate[src] * 100, label=src, color=color)
    ax2.set_ylabel("error rate %")
    ax2.set_xlabel("date")
    ax2.grid(alpha=0.3)
    ax2.legend()
    chart = _save(fig, "01_dataset_health.png")

    return {
        "chart": chart,
        "total_runs": int(daily_runs.sum()),
        "days_covered": int(daily_runs.size),
        "runs_per_day_median": float(daily_runs.median()),
        "milp_total_err_rate": float(
            resp[resp["source"] == "milp"]["status"].eq("error").mean()
        ),
        "ml_total_err_rate": float(
            resp[resp["source"] == "ml"]["status"].eq("error").mean()
        ),
    }


def section_2_objective_agreement(paired: pd.DataFrame) -> dict:
    """Paired objective_value comparison: scatter + histogram."""
    both = paired[(paired.milp_status == "Optimal") & (paired.ml_status == "Optimal")].copy()
    both = both.dropna(subset=["milp_objective", "ml_objective"])

    fig, axes = plt.subplots(1, 2, figsize=(14, 5))

    ax = axes[0]
    ax.scatter(both.milp_objective, both.ml_objective, s=2, alpha=0.2, color=COLOR_MILP)
    lo = min(both.milp_objective.min(), both.ml_objective.min())
    hi = max(both.milp_objective.max(), both.ml_objective.max())
    ax.plot([lo, hi], [lo, hi], "--", color="grey", linewidth=1, label="identity")
    ax.set_xlabel("MILP objective_value (€)")
    ax.set_ylabel("ML objective_value (€)")
    ax.set_title("Section 2 — MILP vs ML, paired Optimal")
    ax.legend()
    ax.grid(alpha=0.3)

    ax = axes[1]
    deltas = both.obj_delta.dropna()
    # log y so the long tail is visible
    ax.hist(deltas.clip(-1, 1), bins=200, color=COLOR_ML, alpha=0.7)
    ax.set_yscale("log")
    ax.set_xlabel("ml − milp (€, clipped to ±1)")
    ax.set_ylabel("# runs (log)")
    ax.set_title("delta histogram")
    ax.grid(alpha=0.3)

    chart = _save(fig, "02_objective_agreement.png")

    return {
        "chart": chart,
        "paired_optimal": int(len(both)),
        "agree_within_1e4": int((both.obj_abs_delta < 1e-4).sum()),
        "agree_within_1e2": int((both.obj_abs_delta < 1e-2).sum()),
        "diverge_ge_0_1": int((both.obj_abs_delta >= 0.1).sum()),
        "mean_signed_delta": float(deltas.mean()),
        "mean_abs_delta": float(deltas.abs().mean()),
    }


def section_3_plan_agreement(paired: pd.DataFrame, db_path: str) -> dict:
    """Slot-by-slot agreement on grid_import/grid_export + battery trajectories.

    For each paired-Optimal run, load both response JSONs and compute
    aggregate planned grid import/export plus planned battery cycling, then
    chart the distributions of MILP-vs-ML deltas.
    """
    both = paired[(paired.milp_status == "Optimal") & (paired.ml_status == "Optimal")]
    # Sample to keep runtime reasonable (40k runs × 2 JSON loads = slow).
    sample = both.sample(min(2000, len(both)), random_state=42)

    deltas = []  # one row per run
    for r in sample.itertuples():
        try:
            milp = loader.load_responses_for_run(r.run_id, db_path).get("milp")
            ml = loader.load_responses_for_run(r.run_id, db_path).get("ml")
        except KeyError:
            continue
        if not milp or not ml:
            continue

        milp_gi = sum(milp.get("grid_import", []) or [])
        ml_gi = sum(ml.get("grid_import", []) or [])
        milp_ge = sum(milp.get("grid_export", []) or [])
        ml_ge = sum(ml.get("grid_export", []) or [])

        # Battery cycling: total charge + total discharge across all batteries
        def _battery_cycling(resp):
            tot = 0.0
            for b in resp.get("batteries", []) or []:
                tot += sum(b.get("charging_power", []) or [])
                tot += sum(b.get("discharging_power", []) or [])
            return tot

        deltas.append(
            {
                "run_id": r.run_id,
                "ts": r.ts,
                "grid_import_milp": milp_gi,
                "grid_import_ml": ml_gi,
                "grid_export_milp": milp_ge,
                "grid_export_ml": ml_ge,
                "bat_cycle_milp": _battery_cycling(milp),
                "bat_cycle_ml": _battery_cycling(ml),
                "obj_delta": ml.get("objective_value", 0) - milp.get("objective_value", 0),
            }
        )

    df = pd.DataFrame(deltas)
    df["grid_import_delta"] = df.grid_import_ml - df.grid_import_milp
    df["grid_export_delta"] = df.grid_export_ml - df.grid_export_milp
    df["bat_cycle_delta"] = df.bat_cycle_ml - df.bat_cycle_milp

    fig, axes = plt.subplots(1, 3, figsize=(15, 4))
    for ax, col, title in [
        (axes[0], "grid_import_delta", "Σ planned grid import (ml − milp)"),
        (axes[1], "grid_export_delta", "Σ planned grid export (ml − milp)"),
        (axes[2], "bat_cycle_delta", "Σ planned battery cycling (ml − milp)"),
    ]:
        ax.hist(df[col], bins=100, color=COLOR_ML, alpha=0.7)
        ax.set_yscale("log")
        ax.set_xlabel(col + " (Wh aggregated)")
        ax.set_ylabel("# runs (log)")
        ax.set_title(title)
        ax.grid(alpha=0.3)
    fig.suptitle("Section 3 — plan-level agreement (sample of 2000 paired-Optimal runs)")
    chart = _save(fig, "03_plan_agreement.png")

    return {
        "chart": chart,
        "sample_size": int(len(df)),
        "grid_import_delta_median_wh": float(df.grid_import_delta.median()),
        "grid_import_delta_p95_wh": float(df.grid_import_delta.abs().quantile(0.95)),
        "bat_cycle_delta_median_wh": float(df.bat_cycle_delta.median()),
        "bat_cycle_delta_p95_wh": float(df.bat_cycle_delta.abs().quantile(0.95)),
        "frac_identical_grid_import": float(
            (df.grid_import_delta.abs() < 1).mean()
        ),
    }


def section_4_whole_house_flows(runs: pd.DataFrame, resp: pd.DataFrame, db_path: str) -> dict:
    """Aggregate planned routing of each backend across all Optimal runs.

    For each backend on a sampled set of runs, sum planned grid import / export
    and battery charge / discharge. Pull solar forecast from the request to
    estimate planned PV self-consumption: pv_planned_self = pv_forecast − grid_export.
    """
    resp_opt = resp[resp.status == "Optimal"].dropna(subset=["response"])
    sample_ids = resp_opt.run_id.unique()
    rng = np.random.default_rng(42)
    if len(sample_ids) > 1500:
        sample_ids = rng.choice(sample_ids, 1500, replace=False)

    rows = []
    runs_indexed = runs.set_index("run_id")
    for rid in sample_ids:
        try:
            req = runs_indexed.loc[rid, "request"]
        except KeyError:
            continue
        if not req:
            continue
        ts_series = req.get("time_series", {})
        ft = ts_series.get("ft", []) or []
        gt = ts_series.get("gt", []) or []
        # ft and gt are both Wh per slot (verified against raw request samples)
        pv_forecast_wh = sum(ft)
        home_forecast_wh = sum(gt)

        for src in ("milp", "ml"):
            rec = resp_opt[(resp_opt.run_id == rid) & (resp_opt.source == src)]
            if rec.empty:
                continue
            r = rec.iloc[0]["response"]
            if not r:
                continue
            gi = sum(r.get("grid_import", []) or [])
            ge = sum(r.get("grid_export", []) or [])
            bc = sum(
                sum(b.get("charging_power", []) or [])
                for b in (r.get("batteries", []) or [])
            )
            bd = sum(
                sum(b.get("discharging_power", []) or [])
                for b in (r.get("batteries", []) or [])
            )
            rows.append(
                {
                    "run_id": rid,
                    "source": src,
                    "pv_forecast_wh": pv_forecast_wh,
                    "home_forecast_wh": home_forecast_wh,
                    "grid_import_wh": gi,
                    "grid_export_wh": ge,
                    "battery_charge_wh": bc,
                    "battery_discharge_wh": bd,
                    "planned_self_consumed_pv_wh": max(0, pv_forecast_wh - ge),
                }
            )

    df = pd.DataFrame(rows)
    summary = df.groupby("source").mean(numeric_only=True)

    # Stacked bar: planned flow pathways per backend (means across sampled runs)
    fig, ax = plt.subplots(figsize=(10, 5))
    metrics = [
        "grid_import_wh",
        "grid_export_wh",
        "battery_charge_wh",
        "battery_discharge_wh",
        "planned_self_consumed_pv_wh",
    ]
    x = np.arange(len(metrics))
    width = 0.35
    if "milp" in summary.index:
        ax.bar(x - width / 2, summary.loc["milp", metrics], width, label="MILP", color=COLOR_MILP)
    if "ml" in summary.index:
        ax.bar(x + width / 2, summary.loc["ml", metrics], width, label="ML", color=COLOR_ML)
    ax.set_xticks(x)
    ax.set_xticklabels([m.replace("_wh", "").replace("_", "\n") for m in metrics])
    ax.set_ylabel("mean Wh per run (planned over horizon)")
    ax.set_title("Section 4 — planned whole-house flows (sample of 1500 runs)")
    ax.legend()
    ax.grid(axis="y", alpha=0.3)
    chart = _save(fig, "04_whole_house_flows.png")

    return {
        "chart": chart,
        "sample_runs": int(df.run_id.nunique()),
        "milp_means": summary.loc["milp"].to_dict() if "milp" in summary.index else {},
        "ml_means": summary.loc["ml"].to_dict() if "ml" in summary.index else {},
    }


def section_5_latency_reliability(resp: pd.DataFrame) -> dict:
    """Duration p50/p95/p99 per backend + error breakdown."""
    ok = resp[resp.status == "Optimal"]
    pct = ok.groupby("source").duration_ms.quantile([0.5, 0.95, 0.99]).unstack()
    pct.columns = ["p50", "p95", "p99"]

    errs = resp[resp.status == "error"].copy()
    errs["err_class"] = errs.error.apply(_classify_error)
    err_counts = errs.groupby(["source", "err_class"]).size().unstack(fill_value=0)

    fig, axes = plt.subplots(1, 2, figsize=(14, 4.5))

    ax = axes[0]
    sources = ["milp", "ml"]
    width = 0.25
    x = np.arange(len(sources))
    for i, q in enumerate(["p50", "p95", "p99"]):
        vals = [pct.loc[s, q] if s in pct.index else 0 for s in sources]
        ax.bar(x + (i - 1) * width, vals, width, label=q)
    ax.set_xticks(x)
    ax.set_xticklabels(sources)
    ax.set_ylabel("duration_ms")
    ax.set_title("Section 5 — solve latency (Optimal only)")
    ax.legend()
    ax.grid(axis="y", alpha=0.3)

    ax = axes[1]
    err_counts.plot(kind="bar", stacked=True, ax=ax)
    ax.set_title("error breakdown")
    ax.set_ylabel("# errors")
    ax.set_xlabel("source")
    ax.grid(axis="y", alpha=0.3)

    chart = _save(fig, "05_latency_reliability.png")

    return {
        "chart": chart,
        "latency_pct": pct.to_dict(orient="index"),
        "error_counts": err_counts.to_dict(orient="index"),
    }


def section_6_divergence_forensics(paired: pd.DataFrame, db_path: str) -> dict:
    """Inspect runs where |obj_delta| >= 0.1."""
    both = paired[(paired.milp_status == "Optimal") & (paired.ml_status == "Optimal")]
    diverge = both[both.obj_abs_delta >= 0.1].sort_values("obj_abs_delta", ascending=False)

    rows = []
    for r in diverge.itertuples():
        responses = loader.load_responses_for_run(r.run_id, db_path)
        milp = responses.get("milp") or {}
        ml = responses.get("ml") or {}
        rows.append(
            {
                "run_id": r.run_id,
                "ts": r.ts,
                "horizon": r.horizon,
                "milp_obj": r.milp_objective,
                "ml_obj": r.ml_objective,
                "obj_delta": r.obj_delta,
                "milp_grid_imp": sum(milp.get("grid_import", []) or []),
                "ml_grid_imp": sum(ml.get("grid_import", []) or []),
                "milp_grid_exp": sum(milp.get("grid_export", []) or []),
                "ml_grid_exp": sum(ml.get("grid_export", []) or []),
            }
        )

    df = pd.DataFrame(rows)

    # Timeline of divergent runs
    fig, ax = plt.subplots(figsize=(12, 4))
    if not df.empty:
        ax.scatter(df.ts, df.obj_delta.abs(), c=COLOR_ML, s=30)
        ax.set_yscale("log")
        ax.set_ylabel("|objective delta|")
        ax.set_xlabel("ts")
        ax.set_title(f"Section 6 — {len(df)} divergent runs (|Δobj| ≥ 0.1)")
        ax.grid(alpha=0.3)
    chart = _save(fig, "06_divergence_timeline.png")

    csv_path = Path(__file__).parent / "06_divergent_runs.csv"
    df.to_csv(csv_path, index=False)

    return {
        "chart": chart,
        "csv": str(csv_path),
        "n_divergent": int(len(df)),
        "top_3": df.head(3).to_dict(orient="records") if not df.empty else [],
    }


def section_7_time_of_day(paired: pd.DataFrame) -> dict:
    """Objective + divergence by hour of day (Europe/Berlin)."""
    both = paired[(paired.milp_status == "Optimal") & (paired.ml_status == "Optimal")].copy()
    both["hour"] = both.ts.dt.tz_convert("Europe/Berlin").dt.hour

    fig, axes = plt.subplots(1, 2, figsize=(14, 4.5))

    ax = axes[0]
    by_hour = both.groupby("hour")[["milp_objective", "ml_objective"]].mean()
    ax.plot(by_hour.index, by_hour.milp_objective, label="MILP", color=COLOR_MILP)
    ax.plot(by_hour.index, by_hour.ml_objective, label="ML", color=COLOR_ML)
    ax.set_xlabel("hour of day (Europe/Berlin)")
    ax.set_ylabel("mean objective_value (€)")
    ax.set_title("Section 7 — objective vs time of day")
    ax.legend()
    ax.grid(alpha=0.3)

    ax = axes[1]
    by_hour_div = both.groupby("hour").obj_abs_delta.mean()
    ax.plot(by_hour_div.index, by_hour_div.values, color="black")
    ax.set_yscale("log")
    ax.set_xlabel("hour of day")
    ax.set_ylabel("mean |objective delta| (€)")
    ax.set_title("divergence by hour")
    ax.grid(alpha=0.3)

    chart = _save(fig, "07_time_of_day.png")
    return {"chart": chart}


def section_8_input_sanity(runs: pd.DataFrame) -> dict:
    """Sample 20 runs; plot solar forecast (ft) and home profile (gt) to confirm
    both backends are seeing well-formed inputs."""
    sample = runs.dropna(subset=["request"]).sample(20, random_state=42)

    fig, axes = plt.subplots(2, 1, figsize=(12, 6), sharex=True)
    for r in sample.itertuples():
        ts = r.request.get("time_series", {})
        ft = ts.get("ft", []) or []
        gt = ts.get("gt", []) or []
        if ft:
            axes[0].plot(ft, alpha=0.4)
        if gt:
            axes[1].plot(gt, alpha=0.4)
    axes[0].set_ylabel("ft (solar forecast, kWh/slot)")
    axes[0].set_title("Section 8 — 20 random request inputs")
    axes[0].grid(alpha=0.3)
    axes[1].set_ylabel("gt (home profile, Wh/slot)")
    axes[1].set_xlabel("slot index")
    axes[1].grid(alpha=0.3)

    chart = _save(fig, "08_input_sanity.png")
    return {"chart": chart, "sample_size": int(len(sample))}


def _classify_error(msg: str) -> str:
    if not msg:
        return "unknown"
    m = msg.lower()
    if "tls" in m or "x509" in m:
        return "tls"
    if "lookup" in m or "dial tcp" in m:
        return "network"
    if "context deadline" in m or "timeout" in m:
        return "timeout"
    if "http 401" in m or "http 403" in m:
        return "auth"
    if "http 400" in m:
        return "bad_request"
    if "http 4" in m or "http 5" in m:
        return "http_other"
    if "empty response" in m:
        return "empty_body"
    if m.startswith("panic"):
        return "panic"
    if "solver status" in m:
        return "non_optimal"
    return "other"


def main(db_path: str | Path) -> dict:
    db_path = str(db_path)
    print(f"Loading {db_path}...")
    summary = loader.db_summary(db_path)
    print(json.dumps(summary, indent=2, default=str))

    print("Loading runs + responses (this may take ~30s for 40k runs)...")
    runs = loader.load_runs(db_path, parse_json=True)
    resp = loader.load_responses(db_path, parse_json=True)
    paired = loader.load_paired(db_path)

    out = {"db_summary": summary}
    print("Section 1...");  out["s1"] = section_1_dataset_health(runs, resp)
    print("Section 2...");  out["s2"] = section_2_objective_agreement(paired)
    print("Section 3 (slow, samples 2000 runs)...");  out["s3"] = section_3_plan_agreement(paired, db_path)
    print("Section 4 (slow, samples 1500 runs)...");  out["s4"] = section_4_whole_house_flows(runs, resp, db_path)
    print("Section 5...");  out["s5"] = section_5_latency_reliability(resp)
    print("Section 6...");  out["s6"] = section_6_divergence_forensics(paired, db_path)
    print("Section 7...");  out["s7"] = section_7_time_of_day(paired)
    print("Section 8...");  out["s8"] = section_8_input_sanity(runs)

    out_path = Path(__file__).parent / "analyze_results.json"
    out_path.write_text(json.dumps(out, indent=2, default=str))
    print(f"\nWrote summary to {out_path}")
    print(f"Charts under {CHARTS}")
    return out


if __name__ == "__main__":
    db = sys.argv[1] if len(sys.argv) > 1 else str(loader.DEFAULT_DB)
    main(db)
