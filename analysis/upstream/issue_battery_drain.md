# evcc bug report draft — paste into https://github.com/evcc-io/evcc/issues/new?template=bug_report.yaml

## Title

House battery is drained into the car below the configured "battery usage" buffer level

## Describe the bug

I use a house battery together with the "Battery usage" settings:

- Prioritize home battery charging until it reaches 25% (prioritySoc)
- Battery-supported vehicle charging when home battery is above 45% (bufferSoc)
- Start automatically when above 60% (bufferStartSoc)

My expectation as a user: below 45% the house battery is reserved for the
house and is not used to charge the car anymore.

What actually happens: once charging is running, evcc keeps drawing the
minimum charging power from the house battery even after the battery has
fallen below the 45% buffer. In **Min+Solar** mode the car simply keeps
charging at minimum power with no sun, draining the battery below the buffer
towards the priority limit. The same drain happens during **fast/plan
charging**. So the buffer only controls when battery-supported charging may
*start*, but it never *stops* it — which is very surprising when you have
configured "use the battery for the car only above 45%".

I first noticed this on my production system (HomeAssistant add-on, go-e
Charger V3, Renault ZOE, 15 kWh battery): in the evening the car kept charging
at minimum power until the battery was down at the priority limit. Because
that depends on the weather it is hard to reproduce on demand, so I scripted a
reproduction against current **master** using simulated meters (script-plugin
meters fed from files; the control loop is unmodified evcc). The log below is
from that run and shows the issue deterministically.

I have been running a patched version on my own system since April where the
battery is put on hold during fast charging below the buffer level and
Min+Solar stops guaranteeing minimum current below the buffer level. That
behaves as I would expect. I would like to contribute this as a pull request
(branch with tests is prepared).

## Steps to reproduce

1. Configure a house battery and set buffer (45%), buffer start (60%),
   priority (25%) in "Battery usage".
2. Connect a car, select Min+Solar mode. Optionally set a charging plan
   (here: next day 06:30, 75%).
3. No solar surplus (evening). evcc starts minimum-current charging, supplied
   by the house battery.
4. Let the battery fall below 45%: charging continues at minimum power and
   the battery keeps discharging into the car (log below: 55% → 42%).

Reproduction rig: meters of `type: custom` with `source: script` reading
values from files, so battery SoC can be moved while evcc runs; unmodified
evcc from master. The complete package is attached — `repro-evcc.yaml`
(config), `repro-driver.sh` (timeline: settings + plan via API, car reaction,
1%-per-123s battery physics for 15 kWh @ 4.39 kW) and the full log — so the
run can be replayed as-is in about 30 minutes.

## Configuration details

```yaml
interval: 10s
site:
  title: repro
  meters: { grid: grid, pv: pv, battery: bat }
meters:
  - name: grid
    type: custom
    power: { source: script, cmd: cat /tmp/repro/grid_power }
  - name: pv
    type: custom
    power: { source: script, cmd: cat /tmp/repro/pv_power }
  - name: bat
    type: custom
    power: { source: script, cmd: cat /tmp/repro/battery_power }
    soc: { source: script, cmd: cat /tmp/repro/battery_soc }
    capacity: 15
chargers:
  - name: wallbox
    type: custom
    status: { source: script, cmd: cat /tmp/repro/charger_status }
    enabled: { source: script, cmd: cat /tmp/repro/charger_enabled }
    enable: { source: script, cmd: sh -c 'echo {{.enable}} > /tmp/repro/charger_enabled' }
    maxcurrent: { source: script, cmd: sh -c 'echo {{.maxcurrent}} > /tmp/repro/charger_maxcurrent' }
vehicles:
  - name: ev
    type: custom
    title: Zoe
    capacity: 52
    soc: { source: script, cmd: cat /tmp/repro/vehicle_soc }
loadpoints:
  - title: Stellplatz
    charger: wallbox
    vehicle: ev
    mode: minpv
# battery usage set via API after start:
#   POST /api/buffersoc/45, /api/bufferstartsoc/60, /api/prioritysoc/25
#   POST /api/vehicles/ev/plan/soc/75/2026-07-29T06:30:00+02:00
```

## Log details

Excerpt (full log attached). 15 kWh battery discharging 4390 W (car 4140 W at
min current + house 250 W) drains 1% per ~2 minutes; the run covers ~28
minutes from connect to 42%. The vehicle plan (next day 06:30, target 75%) is
active the whole time.

```
# 09:12 vehicle connected, no PV, Min+Solar - evcc guarantees min current
[lp-1  ] INFO 2026/07/28 09:12:37 car connected
[site  ] DEBUG 2026/07/28 09:12:37 battery 1 soc: 55%
[site  ] DEBUG 2026/07/28 09:12:37 site power: 250W
[lp-1  ] DEBUG 2026/07/28 09:12:37 pv charge current: min 6A > 0A (250W @ 3p, battery: false)
[lp-1  ] DEBUG 2026/07/28 09:13:08 plan: charge 49m52s between 2026-07-29 05:40:08 +0200 CEST until 2026-07-29 06:30:00 +0200 CEST (power: 11040W, avg cost: 0.000)
# 09:14 charging starts, fully supplied by the house battery (grid stays 0W)
[site  ] DEBUG 2026/07/28 09:14:08 battery 1 soc: 55%
[site  ] DEBUG 2026/07/28 09:14:08 site power: 4390W
[lp-1  ] DEBUG 2026/07/28 09:14:08 charger status: C
[lp-1  ] DEBUG 2026/07/28 09:14:08 pv charge current: min 6A > 0A (4390W @ 3p, battery: true)
# battery drains ~1%/2min: 50% after 10 minutes ...
[site  ] DEBUG 2026/07/28 09:24:07 battery 1 soc: 50%
[lp-1  ] DEBUG 2026/07/28 09:24:07 session energy: 0.690kWh
[lp-1  ] DEBUG 2026/07/28 09:24:08 pv charge current: min 6A > 0A (4390W @ 3p, battery: true)
# ... 46% after 20 minutes, still charging at min current
[site  ] DEBUG 2026/07/28 09:32:38 battery 1 soc: 46%
[lp-1  ] DEBUG 2026/07/28 09:32:38 session energy: 1.276kWh
[lp-1  ] DEBUG 2026/07/28 09:32:38 pv charge current: min 6A > 0A (4390W @ 3p, battery: true)
# 09:34 battery reaches the 45% buffer - evcc keeps charging from it
[site  ] DEBUG 2026/07/28 09:34:38 battery 1 soc: 45%
[site  ] DEBUG 2026/07/28 09:34:38 site power: 4390W
[lp-1  ] DEBUG 2026/07/28 09:34:38 charger status: C
[lp-1  ] DEBUG 2026/07/28 09:34:38 pv charge current: min 6A > 0A (4390W @ 3p, battery: false)
[site  ] DEBUG 2026/07/28 09:36:38 battery 1 soc: 44%
[lp-1  ] DEBUG 2026/07/28 09:36:38 session energy: 1.553kWh
[lp-1  ] DEBUG 2026/07/28 09:36:38 pv charge current: min 6A > 0A (4390W @ 3p, battery: false)
[site  ] DEBUG 2026/07/28 09:38:38 battery 1 soc: 43%
[lp-1  ] DEBUG 2026/07/28 09:38:38 pv charge current: min 6A > 0A (4390W @ 3p, battery: false)
[site  ] DEBUG 2026/07/28 09:40:38 battery 1 soc: 42%
[site  ] DEBUG 2026/07/28 09:40:38 site power: 4390W
[lp-1  ] DEBUG 2026/07/28 09:40:38 charger status: C
[lp-1  ] DEBUG 2026/07/28 09:40:38 session energy: 1.828kWh
[lp-1  ] DEBUG 2026/07/28 09:40:38 plan: charge 39m56s between 2026-07-29 05:50:04 +0200 CEST until 2026-07-29 06:30:00 +0200 CEST (power: 11040W, avg cost: 0.000)
[lp-1  ] DEBUG 2026/07/28 09:40:38 pv charge current: min 6A > 0A (4390W @ 3p, battery: false)
```

Note the section from 09:34: battery at and below the 45% buffer
("battery: false"), charger still status C, still 4390 W minimum-current
charging supplied by the battery — 1.8 kWh drained from the battery into the
car in half an hour, and it would continue down to prioritySoc.

## What type of operating system or environment does evcc run on?

HomeAssistant Add-on
(production; the attached reproduction log was captured on macOS with an
unmodified build of current master)

## External automation

- [x] I have made sure that no external automation like HomeAssistant or
      Node-RED is active or accessing any of the mentioned devices when this
      issue occurs.

## Nightly build

- [x] I have verified that the issue is reproducible with the latest nightly
      build (reproduced with an unmodified build of current master,
      commit 1f0aaf14b, log above)

## Version

master (1f0aaf14b) for the reproduction; also present on 0.312.1 and on my
production system.
