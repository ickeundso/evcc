# evcc feature request draft — paste into https://github.com/evcc-io/evcc/issues/new?template=feature_request.md
# Issue 2 of 2: the configuration wizard (for the separate wizard PR).
# File AFTER the readings issue/PR (#32219) — the wizard PR will be rebased on it.

## Title

Viessmann: discover installation id and gateway serial in the config UI instead of curl+jq instructions

**Is your feature request related to a problem? Please describe.**

Setting up the Viessmann heat pump requires an `installation_id` that cannot
be looked up in any Viessmann app. The template's help openly admits: "we're
aware this is not for every user, but currently we don't have a better
workflow" — and then walks the user through a ~30-line command-line procedure
with curl, jq, a hand-crafted OAuth PKCE flow, and an intermediate code that
expires after 20 seconds. For most users this is the hardest step of the
whole setup, and easy to get wrong.

The strange part: evcc already performs this exact OAuth login through its
`viessmann` auth provider — the user logs in via the UI anyway. The manual
curl dance only re-does what evcc has already done, just to read one number.

**Describe the solution you'd like**

After the OAuth login, the Viessmann API lists everything the user is asked
to find out by hand:

```
GET /iot/v2/equipment/installations?includeGateways=true
```

evcc already has the right mechanism for this: template params with a
`service:` — the same pattern the tibber vehicle template uses for VIN
selection. Concretely:

- a `viessmann` config service with two endpoints,
  `viessmann/installations` and `viessmann/gateways`, backed by the
  equipment API and reusing the already-authorized OAuth instance for the
  same client id (exactly like `vehicle/tibber/service.go`),
- `installation_id` and `gateway_serial` template params annotated with
  `service:`, so the config UI offers the values as a dropdown after login
  and auto-fills them when the account has a single installation/gateway,
- the curl+jq instructions collapsed into a `<details>` fallback in the help.

No frontend changes are needed — the existing service-param pipeline
(fetch after login, dropdown, auto-fill single values) handles everything.

I have implemented this and tested it against my live system (Vitocal 250-A):
after entering the client id and logging in, gateway serial and installation
id fill themselves. The setup reduces to "client id + one login click".
A pull request is prepared.

<!-- attach screenshot of the auto-filled fields here -->

**Describe alternatives you've considered**

- Startup discovery with optional params (like `ensureVehicle()` does for
  VINs): also possible, but the config-UI service approach gives immediate
  feedback during setup and needs no changes to instance creation. Both could
  be combined later.
- Simplifying the curl instructions: still requires a terminal and fails for
  non-technical users.

**Additional context**

Related: #32219 (Viessmann template readings — independent change, separate
PR by design: template-only data fix vs. new config-service code).
