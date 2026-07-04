# ML Training Brief — What to Build When

**Author:** working session 2026-07-03, based on `report.md` (Track A) + `outcome_report.md` (Track B).
**Audience:** future me / anyone taking over `evcc-ml-optimizer`.

## Where we are

- 84 days of paired MILP + ML responses (50k+ runs) show the two solvers agree
  to 4 decimal places when both return Optimal — expected, because the ML side
  is a rule-clone of MILP.
- 10 days of aggregator outcomes with whole-house coverage now exist. Data
  shape works end-to-end. The volume is nowhere near enough to train a real
  model.
- Response-derived predicted grid cost vs actual: MAE ~26 €, dominated by
  solar-forecast error (p90 25 kWh over the horizon), not the solver.
- The user's house is 99% autark / 70% self-consuming in summer. Any ML
  approach targeting "grid cost only" is optimizing something already close
  to zero and will find no signal.

## What NOT to train first

**Do not train a "smarter MILP" model on the current dataset.**

- The ML target you'd naturally reach for — `objective_value` — is not grid
  cost (it includes battery terminal value + cycling penalties). Training on
  it just teaches the model to reproduce MILP's opaque scalar.
- Even the response-derived predicted grid cost is a lossy compression of
  what MILP does. MILP's real output is a **plan sequence** — per-slot
  arrays of `grid_import`, `grid_export`, `charging_power`, `discharging_power`
  per battery. Regressing to a scalar throws that away.
- With 10 days of full-house data (≈ 6,000 outcomes), you have too little
  signal for any transformer/RNN and just enough to overfit a small MLP into
  hallucinating patterns that aren't there.

**Do not train a battery-schedule policy yet.**

- Ground truth for actual battery decisions is per-loadpoint-and-battery, but
  the aggregator captures only the aggregate. Attributing observed grid /
  battery flows back to "should MILP have discharged more?" needs a
  counterfactual simulator the current dataset can't support.

## What to build first — three sequential steps

### Step 1: A PV forecast model (biggest lever, smallest surface)

The single most impactful improvement is a **better solar forecast**. Median
absolute error today is 9 kWh over the horizon; p90 is 25 kWh. Cut that in
half and you cut the plan-vs-actual gap by more than the solver ever can.

**Signal we already have**
- 84 days of `time_series.ft` (evcc's current solar forecast, per slot)
- 10 days of `actual_pv_wh` per outcome (ground truth per horizon window)
- Timestamp → hour of day / day of year → azimuth / declination features
- Recent-history PV: read the last N slots' `actual_pv_wh` from the collector
  before the run and include them in the request payload

**Signal to add**
- Local weather API forecast (cloud cover %, temperature, wind — free tier
  from OpenWeatherMap or DWD) — this is the biggest gap
- Persistence baseline: "next slot = last slot" and "next N slots = same
  hours yesterday" as a strong baseline every model must beat

**Model**
- Start with **gradient-boosted regression** (LightGBM or XGBoost) per-slot,
  predicting a scalar Wh
- Features: hour, day-of-year, cloud cover forecast, temperature, last 12
  slots of actual PV, evcc's current ft as a feature (not a target)
- Target: `actual_pv_wh` per slot from the outcomes table
- Evaluate: MAE + p50/p90 abs error, per hour of day (loss is worse midday)
- **Dataset needed**: 6–12 weeks of full-house data — the aggregator gathers
  ~600 outcomes / day, so we're at ~6k rows after 10 days, need ~30k

**Where it plugs in**
- New evcc plugin: `tariff/solar/ml.go` (like `tariff/solar/self.go` but calls
  the ML service)
- Substitutes evcc's `ft` in the optimizer request — MILP is unchanged,
  becomes a MILP-driven-by-better-forecast

### Step 2: A house-load forecast model (smaller lever, same surface)

Home consumption forecast (`gt`) is systematically ~2 kWh too high per
horizon. Same recipe as step 1 but with `actual_home_wh` as target and
day-of-week / time-of-day features. Easier to model (weekly patterns are
strong), less impactful (home load is smaller than PV in summer).

### Step 3 (if 1 and 2 land value): a plan-imitation model

Only after PV/home forecasts are good, consider training an ML model to
imitate MILP's plans directly.

**Framing**
- Input: the full `OptimizationInput` (all fields, all slots)
- Output: the full plan (`grid_import[]`, `grid_export[]`, per-battery
  `charging_power[]`, `discharging_power[]`, `state_of_charge[]`)
- Target: the MILP response we've been logging

**Why**
- MILP is 300–20,000 ms on the RPi with a p99 of 19.8 s (see
  `analysis/ab/report.md`, section 5). ML proxy is 3× faster at p99. A real
  ML model would let evcc replan every 30 s instead of every 2 min → much
  faster response to sudden PV changes.

**Why later**
- Needs 30k+ good MILP examples; we have ~40k paired but the schema quality
  varies (see the objective_value confusion). Clean labels take work.
- Model architecture is non-trivial: variable horizon (46–138 slots), 1+
  batteries per site, structured output.
- Payoff is speed, not accuracy — MILP is already very good, ML at best
  matches it. That's fine if it's 100× faster and can be replan-online, but
  it's a smaller win than a better forecast.

## Dataset packaging (for whichever step)

`analysis/ab/loader.py` already gives you `load_paired_with_outcomes()`
returning a DataFrame with runs + responses + outcomes joined. Extend it with
`load_run_features()` that also unpacks per-slot arrays from the request /
response JSON — that becomes the ML input pipeline.

Recommended split, once ≥ 8 weeks of full-house data exists:

- Train: first 6 weeks
- Val: week 7
- Test: week 8 (unseen, no peeking)

Blocking by week prevents leakage from tomorrow into today.

## First concrete milestone

Ship the aggregator running for 8 more weeks. Come back with:

1. `evcc-ab-export.db` with ≥ 30k full-house outcomes
2. `analysis/ab/pv_baseline.py` — persistence + evcc's current `ft` scored
   on the held-out week (this is the "must beat" number)
3. Then run this brief.

Everything before that is premature. The plumbing (harness + aggregator +
analysis) is now correct. The scientific question needs weeks of clock time,
not more code.

## Open questions worth resolving before step 1

1. **Where do the corrupt meter rows come from?** ~26% of outcomes were
   flagged as physically implausible. If it's a specific device dropout
   pattern, plugging it fixes forecast training silently. Look at the
   distribution of the 64 GWh values in the `meters` table on the RPi.
2. **Are the PV forecast errors weather-correlated, or systematic time-of-day
   / seasonal biases?** If systematic, a simple correction table beats ML.
   Cheap to check first.
3. **Should the ML service just call the weather API + a small model** and
   return the corrected `ft`, or become the whole optimizer? Simpler
   deployment vs bigger scope. Start simple.
