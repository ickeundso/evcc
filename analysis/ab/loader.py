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
