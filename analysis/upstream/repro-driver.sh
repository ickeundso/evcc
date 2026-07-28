#!/bin/sh
SIM=/tmp/repro
cd /path/to/evcc   # unmodified master checkout, built with: make ui build
./evcc --config $SIM/evcc.yaml --database $SIM/evcc.db --disable-auth > $SIM/evcc.log 2>&1 &
EVCC=$!
sleep 20
# battery usage settings + tomorrow's plan
curl -s -X POST http://127.0.0.1:7099/api/buffersoc/45 >/dev/null
curl -s -X POST http://127.0.0.1:7099/api/bufferstartsoc/60 >/dev/null
curl -s -X POST http://127.0.0.1:7099/api/prioritysoc/25 >/dev/null
curl -s -X POST "http://127.0.0.1:7099/api/vehicles/ev/plan/soc/75/2026-07-29T06:30:00%2B02:00" >/dev/null
# a few idle cycles at status B, then evcc enables (min guarantee); wait for it
for i in $(seq 1 24); do
  grep -q true $SIM/charger_enabled && break
  sleep 10
done
sleep 45   # ~1.5 cycles of "enabled but car not started yet"
# car starts charging: 4140W car + 250W house, all from battery
echo C > $SIM/charger_status
echo 4390 > $SIM/battery_power
# physics: 15kWh battery, 4390W discharge -> 1%/123s
for soc in 54 53 52 51 50 49 48 47 46 45 44 43 42; do
  sleep 123
  echo $soc > $SIM/battery_soc
done
# two more cycles below buffer, then end
sleep 75
kill $EVCC
touch $SIM/done
