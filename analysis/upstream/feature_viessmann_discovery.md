# evcc feature request draft — paste into https://github.com/evcc-io/evcc/issues/new?template=feature_request.md

## Title

Viessmann: auto-discover installation id / gateway serial instead of the curl+jq instructions

**Is your feature request related to a problem? Please describe.**

Setting up the Viessmann heat pump today requires an `installation_id` that
cannot be looked up in any Viessmann app. The template's help text openly
says: "we're aware this is not for every user, but currently we don't have a
better workflow" — and then walks the user through a ~30-line command-line
procedure with curl, jq, a hand-crafted OAuth code flow with PKCE challenge
strings, and a code that expires after 20 seconds. For most users this is the
single hardest step of the whole setup, and it is easy to get wrong.

The strange part: evcc already performs the same OAuth login through its
`viessmann` auth provider — the user logs in via the UI anyway. The manual
curl dance only re-does what evcc can already do, just to read one number.

**Describe the solution you'd like**

After the OAuth login that evcc already handles, the Viessmann API can list
everything we ask the user to find out by hand:

```
GET /iot/v2/equipment/installations?includeGateways=true
```

returns the installation id(s) including their gateway serials and devices.

So ideally `installation_id`, `gateway_serial` and `device_id` become
optional: when omitted, evcc discovers them at startup via the authenticated
API and auto-selects if there is exactly one installation/gateway (error with
the list of candidates otherwise, so the user can pin the right one).

evcc already has exactly this pattern for vehicles: `ensureVehicle()` lists
the account's vehicles when no VIN is configured and auto-selects a single
match. The same "configure only if ambiguous" experience would reduce the
Viessmann setup to: client id + one login click.

As a longer-term idea, this could grow into a general guided flow in the
config UI for OAuth-based devices (login → pick installation → done), but the
startup discovery alone would already remove the curl+jq wall entirely.

**Describe alternatives you've considered**

- Keeping the curl+jq instructions but simplifying them — still requires a
  terminal, still fails for most non-technical users.
- A separate helper command (e.g. `evcc token`-style) that prints the
  installation id after login — better, but still an extra manual step that
  discovery at startup makes unnecessary.

**Additional context**

Related: #32219 (Viessmann template readings). I'd be happy to work on the
discovery implementation; it likely needs a small Go device/auth helper for
the Viessmann provider since the plugin-based template cannot make dynamic
parameter lookups.
