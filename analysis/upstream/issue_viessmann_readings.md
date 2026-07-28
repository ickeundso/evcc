# evcc feature request draft — paste into https://github.com/evcc-io/evcc/issues/new?template=feature_request.md
# (feature request, not bug: nothing crashes — the template just never wired the
#  data points, so evcc falls back to its generic power estimate. Feature
#  requests do not require trace logs.)

## Title

Viessmann heat pump: read real power and temperature from the API

**Is your feature request related to a problem? Please describe.**

The Viessmann heat pump template only controls one-time hot water charging
(`heating.dhw.oneTimeCharge`). It does not read any power or temperature
values, although the Viessmann API provides them.

Because no power reading is configured, evcc falls back to its generic
estimate (charging current × phases × 230 V) for the loadpoint display. For a
heat pump this is very misleading: my loadpoint showed up to ~11 kW "charge
power" while the compressor actually drew about 1.3 kW — it looks as if the
electric heating rod is running at full power the whole time. The charged
energy shown is equally meaningless, and the temperature tile stays empty.

<!-- optional: attach the UI screenshot showing the wrong ~11 kW here -->

**Describe the solution you'd like**

The API provides the real values; on my Vitocal 250-A (One Base / E3
generation) the device feature list contains among others:

```
heating.power.consumption.current            1.345 kW, status=connected
heating.dhw.sensors.temperature.dhwCylinder  58.4°C, status=connected
heating.dhw.temperature.main                 55°C (target)
```

The template should read these as `power`, `temp` and `limittemp`, so the
loadpoint shows the true electrical consumption and the hot water temperature
with its target.

Two things to take care of:

- **API quota**: the Viessmann API has a strict daily request quota shared
  with the ViCare app. Polling each feature separately exhausts it within
  hours (HTTP 429). All readers should therefore share one cached request to
  the device feature-list endpoint.
- **Older devices**: which data points a device provides is device-specific
  (the feature endpoint is self-describing, see the
  [developer portal](https://developer.viessmann-climatesolutions.com/)).
  Devices older than the One Base (E3) generation may not provide these
  points, so the readings must be optional and off by default — existing
  installations must be unchanged on upgrade.

I have this running on my own system (Vitocal 250-A) and have prepared a pull
request: #32219.

**Describe alternatives you've considered**

- Configuring a separate physical meter for the heat pump — works, but needs
  extra hardware although the API already provides the value.
- Leaving it as is — the loadpoint then shows the misleading estimated power
  for every Viessmann heat pump.

**Additional context**

Tested live against a Vitocal 250-A. The same data points are reported for
other One Base (E3) devices, e.g. Vitocal 252-A
(home-assistant/core#155695). A companion `usage: aux` meter template makes
the same consumption value usable in the energy flow when the heat pump is
not configured as a charger.
