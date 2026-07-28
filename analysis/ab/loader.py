"""Loader for evcc A/B shadow-evaluation export.

Reads the sidecar SQLite file produced by:

    sqlite3 /addon_configs/97d03a59_evcc_feature/evcc.db \\
        ".dump ab_optimizer_runs ab_optimizer_responses ab_actual_outcomes" \\
        | sqlite3 /share/evcc-ab-export.db

and returns DataFrames with the JSON columns already parsed into Python dicts.

The default DB path is `evcc-ab-export.db` at the repo root.
"""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

import numpy as np
import pandas as pd

DEFAULT_DB = Path(__file__).resolve().parents[2] / "evcc-ab-export.db"


@dataclass
class PairedRow:
    """One row joining MILP and ML responses for the same run."""

    run_id: int
    ts: pd.Timestamp
    horizon: int
    slot_dur_s: int
    milp_status: str
    milp_objective: float | None
    milp_duration_ms: int
    milp_error: str
    ml_status: str
    ml_objective: float | None
    ml_duration_ms: int
    ml_error: str


def _connect(db_path: Path | str = DEFAULT_DB) -> sqlite3.Connection:
    db = Path(db_path)
    if not db.exists():
        raise FileNotFoundError(
            f"export DB not found at {db}; export with sqlite3 .dump first"
        )
    return sqlite3.connect(db)


def load_runs(db_path: Path | str = DEFAULT_DB, parse_json: bool = True) -> pd.DataFrame:
    """Return ab_optimizer_runs as a DataFrame.

    With parse_json=True (default), the `request` column is parsed into dicts.
    With parse_json=False, the raw JSON strings are kept (faster).
    """
    with _connect(db_path) as conn:
        df = pd.read_sql_query(
            "SELECT id AS run_id, ts, horizon, slot_dur_s, request FROM ab_optimizer_runs ORDER BY id",
            conn,
        )
    df["ts"] = pd.to_datetime(df["ts"], utc=True, format="ISO8601")
    if parse_json:
        df["request"] = df["request"].apply(_safe_json)
    return df


def load_responses(db_path: Path | str = DEFAULT_DB, parse_json: bool = True) -> pd.DataFrame:
    """Return ab_optimizer_responses as a DataFrame.

    With parse_json=True (default), the `response` column is parsed into dicts.
    """
    with _connect(db_path) as conn:
        df = pd.read_sql_query(
            """SELECT id AS response_id, run_id, source, ts, duration_ms,
                      status, error, objective_value, response
               FROM ab_optimizer_responses ORDER BY run_id, source""",
            conn,
        )
    df["ts"] = pd.to_datetime(df["ts"], utc=True, format="ISO8601")
    if parse_json:
        df["response"] = df["response"].apply(_safe_json)
    return df


def load_paired(db_path: Path | str = DEFAULT_DB) -> pd.DataFrame:
    """Return one row per run with MILP and ML side by side.

    Excludes runs that don't have both backends recorded. Includes failed
    backends — caller can filter on milp_status / ml_status.
    """
    with _connect(db_path) as conn:
        df = pd.read_sql_query(
            """
            SELECT r.id AS run_id,
                   r.ts AS ts,
                   r.horizon AS horizon,
                   r.slot_dur_s AS slot_dur_s,
                   m.status AS milp_status,
                   m.objective_value AS milp_objective,
                   m.duration_ms AS milp_duration_ms,
                   m.error AS milp_error,
                   l.status AS ml_status,
                   l.objective_value AS ml_objective,
                   l.duration_ms AS ml_duration_ms,
                   l.error AS ml_error
            FROM ab_optimizer_runs r
            JOIN ab_optimizer_responses m
              ON m.run_id = r.id AND m.source = 'milp'
            JOIN ab_optimizer_responses l
              ON l.run_id = r.id AND l.source = 'ml'
            ORDER BY r.id
            """,
            conn,
        )
    df["ts"] = pd.to_datetime(df["ts"], utc=True, format="ISO8601")
    df["obj_delta"] = df["ml_objective"] - df["milp_objective"]
    df["obj_abs_delta"] = df["obj_delta"].abs()
    df["speedup"] = df["milp_duration_ms"] / df["ml_duration_ms"].clip(lower=1)
    return df


def load_outcomes(db_path: Path | str = DEFAULT_DB) -> pd.DataFrame:
    """Return ab_actual_outcomes as a DataFrame.

    Only rows written by the aggregator — historical runs whose window has not
    yet closed do not have an outcome row. Numeric columns are nullable
    (pd.NA when the aggregator couldn't reconstruct the flow for that group).
    """
    with _connect(db_path) as conn:
        df = pd.read_sql_query(
            """SELECT run_id, window_start, window_end,
                      actual_cost, actual_grid_wh, actual_feedin_wh,
                      actual_pv_wh,
                      actual_battery_charge_wh, actual_battery_discharge_wh,
                      actual_home_wh, actual_loadpoint_wh,
                      actual_self_consumed_wh, notes
               FROM ab_actual_outcomes ORDER BY run_id""",
            conn,
        )
    df["window_start"] = pd.to_datetime(df["window_start"], utc=True, format="ISO8601")
    df["window_end"] = pd.to_datetime(df["window_end"], utc=True, format="ISO8601")
    return df


def load_paired_with_outcomes(db_path: Path | str = DEFAULT_DB) -> pd.DataFrame:
    """Paired MILP+ML responses joined with the actual-outcome row.

    Filters to runs where both backends returned Optimal AND an outcome row
    exists. Adds predicted-vs-actual columns per backend for cost.
    """
    with _connect(db_path) as conn:
        df = pd.read_sql_query(
            """
            SELECT r.id AS run_id, r.ts, r.horizon, r.slot_dur_s,
                   m.objective_value AS milp_predicted_cost,
                   m.duration_ms AS milp_duration_ms,
                   l.objective_value AS ml_predicted_cost,
                   l.duration_ms AS ml_duration_ms,
                   o.window_start, o.window_end,
                   o.actual_cost, o.actual_grid_wh, o.actual_feedin_wh,
                   o.actual_pv_wh,
                   o.actual_battery_charge_wh, o.actual_battery_discharge_wh,
                   o.actual_home_wh, o.actual_loadpoint_wh,
                   o.actual_self_consumed_wh, o.notes
            FROM ab_optimizer_runs r
            JOIN ab_optimizer_responses m
              ON m.run_id = r.id AND m.source = 'milp' AND m.status = 'Optimal'
            JOIN ab_optimizer_responses l
              ON l.run_id = r.id AND l.source = 'ml' AND l.status = 'Optimal'
            JOIN ab_actual_outcomes o ON o.run_id = r.id
            WHERE o.actual_cost IS NOT NULL
            ORDER BY r.id
            """,
            conn,
        )
    for c in ("ts", "window_start", "window_end"):
        df[c] = pd.to_datetime(df[c], utc=True, format="ISO8601")
    df["milp_err_cost"] = df["milp_predicted_cost"] - df["actual_cost"]
    df["ml_err_cost"] = df["ml_predicted_cost"] - df["actual_cost"]
    df["milp_abs_err_cost"] = df["milp_err_cost"].abs()
    df["ml_abs_err_cost"] = df["ml_err_cost"].abs()
    return df


def load_run_features(
    db_path: Path | str = DEFAULT_DB, local_tz: str = "Europe/Berlin"
) -> pd.DataFrame:
    """Per-run feature table for ML: forecast aggregates + calendar features + actual targets.

    One row per run (ab_optimizer_runs joined with ab_actual_outcomes). Forecast
    values are summed from the request time_series over the horizon window:
    ft = PV forecast (Wh/slot), gt = home-load forecast (Wh/slot), p_N = grid
    import price, p_E = feed-in price. Calendar features are derived from
    window_start in `local_tz` (PV is driven by local solar time). The actual_*
    columns are the measured targets (nullable where the outcome aggregator
    lacked data for that group).
    """
    runs = load_runs(db_path)  # parses the request JSON
    outcomes = load_outcomes(db_path)
    merged = runs.merge(outcomes, on="run_id", how="inner")

    rows = []
    for r in merged.itertuples():
        req = r.request
        if not req:
            continue
        ts = req.get("time_series", {}) or {}
        ft = ts.get("ft") or []  # PV forecast, Wh/slot
        gt = ts.get("gt") or []  # home-load forecast, Wh/slot
        p_n = ts.get("p_N") or []
        p_e = ts.get("p_E") or []
        rows.append(
            {
                "run_id": r.run_id,
                "ts": r.ts,
                "window_start": r.window_start,
                "horizon": r.horizon,
                "slot_dur_s": r.slot_dur_s,
                "n_slots_fc": len(ft),
                "pv_forecast_wh": float(sum(ft)),
                "home_forecast_wh": float(sum(gt)),
                "price_import_mean": float(np.mean(p_n)) if p_n else np.nan,
                "price_export_mean": float(np.mean(p_e)) if p_e else np.nan,
                "actual_pv_wh": r.actual_pv_wh,
                "actual_home_wh": r.actual_home_wh,
                "actual_grid_wh": r.actual_grid_wh,
                "actual_feedin_wh": r.actual_feedin_wh,
                "actual_loadpoint_wh": r.actual_loadpoint_wh,
            }
        )
    df = pd.DataFrame(rows)
    if df.empty:
        return df

    ws_local = df["window_start"].dt.tz_convert(local_tz)
    df["hour"] = ws_local.dt.hour
    df["dow"] = ws_local.dt.dayofweek
    df["doy"] = ws_local.dt.dayofyear
    df["month"] = ws_local.dt.month
    df["hour_sin"] = np.sin(2 * np.pi * df["hour"] / 24)
    df["hour_cos"] = np.cos(2 * np.pi * df["hour"] / 24)
    df["doy_sin"] = np.sin(2 * np.pi * df["doy"] / 365.25)
    df["doy_cos"] = np.cos(2 * np.pi * df["doy"] / 365.25)
    return df


def load_responses_for_run(
    run_id: int, db_path: Path | str = DEFAULT_DB
) -> dict[str, dict]:
    """Return {'milp': response_dict, 'ml': response_dict} for one run.

    Useful for digging into specific divergent runs.
    """
    with _connect(db_path) as conn:
        rows = pd.read_sql_query(
            "SELECT source, response FROM ab_optimizer_responses WHERE run_id = ?",
            conn,
            params=(run_id,),
        )
    return {r.source: _safe_json(r.response) for r in rows.itertuples()}


def load_request_for_run(run_id: int, db_path: Path | str = DEFAULT_DB) -> dict:
    with _connect(db_path) as conn:
        row = pd.read_sql_query(
            "SELECT request FROM ab_optimizer_runs WHERE id = ?",
            conn,
            params=(run_id,),
        )
    if row.empty:
        raise KeyError(f"run {run_id} not found")
    return _safe_json(row.iloc[0]["request"])


@lru_cache(maxsize=1)
def db_summary(db_path: Path | str = DEFAULT_DB) -> dict:
    """Quick descriptive summary of the export — useful for the notebook header."""
    with _connect(db_path) as conn:
        cur = conn.cursor()
        runs = cur.execute("SELECT COUNT(*), MIN(ts), MAX(ts) FROM ab_optimizer_runs").fetchone()
        by_src = dict(
            cur.execute(
                "SELECT source, COUNT(*) FROM ab_optimizer_responses GROUP BY source"
            ).fetchall()
        )
        by_status = dict(
            cur.execute(
                """SELECT source || ':' || status, COUNT(*)
                   FROM ab_optimizer_responses GROUP BY source, status"""
            ).fetchall()
        )
        outcomes = cur.execute("SELECT COUNT(*) FROM ab_actual_outcomes").fetchone()[0]
    return {
        "runs": runs[0],
        "first_run": runs[1],
        "last_run": runs[2],
        "responses_by_source": by_src,
        "responses_by_source_status": by_status,
        "outcomes": outcomes,
    }


def _safe_json(s):
    """JSON parse that returns None for null/empty inputs instead of raising."""
    if s is None or s == "":
        return None
    try:
        return json.loads(s)
    except (json.JSONDecodeError, TypeError):
        return None
