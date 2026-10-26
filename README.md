# Varde Auto Tournament plugin

This out-of-tree service connects Varde groups to Auto Tournament and can manage
Ready Up's fleet plugin as a typed Varde server addon. It uses Auto Tournament's
HTTP API and Ready Up's documented configuration files; neither project's source
or schemas are included here.

## Requirements

- Varde with the plugin and typed-server-addon APIs enabled.
- A service token named `plugin-autotournament`, bound to the group where this
  plugin is enabled, with the `members`, `steam_ids`, `servers`, `server_addons`,
  and `events` scopes.
- Go 1.26 or newer.
- A persistent, private directory for encrypted plugin state.

Build and run:

```sh
go build -o varde-plugin-autotournament ./cmd/varde-plugin-autotournament
export VARDE_PLUGIN_SECRET="$(openssl rand -hex 32)"
export VARDE_URL="https://varde.example.com"
export VARDE_SERVICE_TOKEN="..."
export STATE_DIR="/var/lib/varde-plugin-autotournament"
export LISTEN="127.0.0.1:7310"
./varde-plugin-autotournament
```

The value of `VARDE_PLUGIN_SECRET` must match the manifest's `secret_env` and
contain at least 32 bytes. Keep it stable: it derives the AES-256-GCM key used
to encrypt saved Auto Tournament tokens and fleet keys. `STATE_DIR` must be
persistent across restarts and readable only by the plugin account. The
`groups.json` file is atomically replaced and written with mode `0600`.

Register `manifest.example.json` with the control plane using `--plugin` or
`VARDE_PLUGINS`. Set `base_url` to the plugin's reachable HTTP(S) address.
Use a firewall or trusted network so only the control plane can call this
service. Do not put real credentials in the manifest.

`AT_ALLOW_PRIVATE` defaults to `false`. Set it to `true` only for an
intentionally private Auto Tournament endpoint, such as a development API on
loopback. The Auto Tournament HTTP client still refuses redirects and uses a
10-second request timeout.

## Configure a group

In the plugin settings, provide the Auto Tournament base URL and API token.
The settings hook validates the URL and pings the API before saving. If the
hidden `fleet_key` is empty, the plugin creates one named `varde-<group_id>`
and returns it to Varde for storage. The API token and fleet key are encrypted
in the plugin's state file as well as protected by Varde's own settings storage.

The Tournaments page lists existing tournaments and links to their Auto
Tournament pages. Creating a tournament imports group members' verified Steam
IDs, creates the requested shuffle or bracket tournament, registers players
where the Auto Tournament workflow requires it, and starts the tournament.
When no members are selected, every group member with a Steam ID is included;
members without one are skipped and listed in the action response.

Auto Tournament is licensed under PolyForm Noncommercial. Self-hosting for a
single group is permitted by that license; operating a hosted multi-client
service requires a separate license from Auto Tournament.

## Ready Up server addon

Ready Up addon management is disabled unless **all three** values are set:

- `READYUP_URL`: HTTPS URL of the pinned Ready Up release ZIP.
- `READYUP_SHA256`: lowercase SHA-256 digest of that ZIP.
- `READYUP_LICENSE`: exactly `noncommercial` or `commercial`, matching the
  applicable Ready Up license terms.

There is no default license choice. If any pin value is missing, startup logs
that addon management is disabled. With all values configured, the plugin
manages `autotournament/readyup` only for servers whose `game_id` is `cs2`.
It requests the `csgo` target, extracts the release ZIP into `game/csgo`, and
supplies `cfg/ReadyUp/fleet.cfg` and `cfg/ReadyUp/license.cfg`. The fleet file
uses Ready Up's documented `key = value` format. A public address is written
only when Varde exposes a mesh address and a matching UDP game port.

Reconciliation runs on startup, after the settings hook, every five minutes,
and on the group's `server.created` SSE event. It does not depend on a
`server.updated` event. Varde exposes the mesh address as optional
`summary.address` on the server response, alongside the server's service and
port information. If that address is not assigned when the server is created,
the five-minute reconciliation pass reads the server again and adds the address
once it becomes available. Addon changes are applied by Varde at the next
server start.

The plugin records the first Ready Up license-acceptance timestamp per server
in its encrypted state so ordinary reconciliations do not change the rendered
specification. It sends an addon PUT only when that rendered specification
changes during the current plugin process. Varde HTTP 400 responses (for
example, an unsupported `csgo` target) are nonfatal; the server is skipped and
the rejection is logged once for that server.

### Driver contract

- Extract the addon archive into `game/csgo`.
- Resolve addon file keys relative to `game/csgo`.
- Convert `gameinfo_search_paths` entries into `Game` lines before the `csgo`
  entry in `gameinfo.gi`.
- Reapply addons at each server start.

## State and operation

Each group's Auto Tournament URL, token, fleet key, and per-server license
timestamps are kept in `STATE_DIR/groups.json`. The token and fleet key use
AES-256-GCM with a key derived by HKDF-SHA256 from `VARDE_PLUGIN_SECRET`; the
associated data is `<group_id>/<field>`. Back up the state file and secret
together. Losing either makes saved credentials unusable.

The service starts on `127.0.0.1:7310` unless `LISTEN` is set. It reconnects
to group event streams with `Last-Event-ID` and exponential backoff. No
Auto Tournament token, service token, plugin secret, or fleet key is written
to logs.

## Development

```sh
go vet ./...
go test -race ./...
test -z "$(gofmt -l .)"
```

The test suite includes the ported Auto Tournament client tests, JWT and
settings validation, action workflow coverage, encrypted-state tests, and
Ready Up reconciliation tests using HTTP test servers.
