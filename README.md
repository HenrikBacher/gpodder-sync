# gpodder-sync

A small, self-hosted podcast sync server for [AntennaPod](https://antennapod.org/), speaking the
gpodder.net v2 API. Single static Go binary, SQLite storage, ~15 MB distroless image, built for Unraid.

**It does no authentication itself.** A reverse proxy (Caddy `basic_auth`, or Caddy + Authelia)
authenticates AntennaPod's basic-auth requests and forwards the username in a header. The server
holds no passwords and refuses every connection that doesn't come from the trusted proxy IP.

## Features

- Subscriptions and episode actions (play position, downloads, deletes) synced across devices
- Subscriptions are shared by all of a user's devices (no gpodder.net "sync groups" needed)
- Users are created automatically the first time the proxy forwards a request for them
- Runs as `nobody:users` (99:100), read-only root filesystem friendly, built-in health check

## How requests are trusted

1. The peer address must be in `GPODDER_SYNC_TRUSTED_PROXIES`, otherwise `403`.
2. The proxy's user header (`Remote-User` by default) must match the username in the URL,
   otherwise `401`.

So the proxy **must** overwrite or strip any client-supplied `Remote-User` header, and nothing but
the proxy may be able to reach the container:

- Don't publish a port. Put the container on the same custom Docker network as Caddy, and only there.
- Give Caddy a fixed IP on that network and trust **that IP only**, never the subnet: any other
  container on a trusted subnet could send the header, and published-port traffic can appear to
  come from the network gateway.

## Unraid

1. Get the image onto the server. Build for amd64 from any machine:
   ```sh
   docker build --platform linux/amd64 --build-arg VERSION=0.1.0 -t gpodder-sync:latest .
   docker save gpodder-sync:latest | ssh root@unraid docker load      # or push to a registry
   ```
2. Copy `unraid/gpodder-sync.xml` to `/boot/config/plugins/dockerMan/templates-user/my-gpodder-sync.xml`,
   set `<Repository>` to your image (`gpodder-sync:latest` for a loaded image, or your registry path),
   then Docker → Add Container → pick **gpodder-sync** from the template list.
3. Set **Network** to Caddy's custom network and **Trusted proxy IP** to Caddy's fixed IP.

The template:

- maps `/mnt/user/appdata/gpodder-sync` → `/data`. The image runs as 99:100, which already owns
  appdata, so no `chown` is needed.
- publishes no port.
- adds hardening Extra Parameters: `--read-only --tmpfs /tmp --cap-drop=ALL --security-opt=no-new-privileges`.
- passes Unraid's `TZ`, which is used for log timestamps.

The image has no shell (distroless), so Unraid's *Console* button won't work. Use
`docker exec gpodder-sync /gpodder-sync user list` instead.

## Proxy setup A: Caddy `basic_auth`

Hash each password (argon2id needs **Caddy 2.11+**):

```sh
docker run --rm caddy:2.11.7 caddy hash-password -a argon2id
```

```caddyfile
podsync.example.com {
	basic_auth argon2id {
		alice $argon2id$v=19$m=47104,t=1,p=1$...
	}
	reverse_proxy gpodder-sync:8080 {
		header_up Remote-User {http.auth.user.id}   # overwrites any client-supplied value
	}
}
```

Keep `basic_auth` without a matcher so it covers every path. Caddy doesn't rate-limit failed logins
by itself; add the CrowdSec bouncer or fail2ban on Caddy's logs.

## Proxy setup B: Caddy + Authelia

Authelia (4.38+) checks AntennaPod's basic auth and returns `Remote-User`.

```yaml
# Authelia configuration.yml
server:
  endpoints:
    authz:
      forward-auth:
        implementation: 'ForwardAuth'
        authn_strategies:
          - name: 'HeaderAuthorization'
            schemes: ['Basic']
          - name: 'CookieSession'
access_control:
  rules:                               # before your general rules; first match wins
    - domain: 'podsync.example.com'
      subject: 'group:podsync'
      policy: 'one_factor'             # basic auth can't satisfy two_factor
    - domain: 'podsync.example.com'
      policy: 'deny'
    - domain: '*.example.com'
      subject: 'group:podsync'
      policy: 'deny'
```

```caddyfile
podsync.example.com {
	route {                                  # keeps the directives in this order
		request_header -Remote-User          # drop client-supplied values first
		forward_auth authelia:9091 {
			uri /api/authz/forward-auth
			copy_headers Remote-User
		}
		reverse_proxy gpodder-sync:8080
	}
}
```

Use a dedicated Authelia user (only in group `podsync`) for AntennaPod rather than your main login.
It works like an app password: it can only reach this domain, and a leaked phone doesn't expose
your SSO account. Authelia's `regulation` settings then rate-limit failed sync logins.
*(Setup B is not yet covered by an end-to-end test.)*

## AntennaPod

Settings → Synchronization → **gpodder.net** → enter your server (`podsync.example.com`) and the
username/password the proxy knows, then pick or create a device.

## Configuration

| Env var / flag | Default | |
|---|---|---|
| `GPODDER_SYNC_TRUSTED_PROXIES` / `-trusted-proxies` | *(required)* | comma-separated proxy IPs/CIDRs |
| `GPODDER_SYNC_PROXY_USER_HEADER` / `-proxy-user-header` | `Remote-User` | header with the username |
| `GPODDER_SYNC_LISTEN` / `-listen` | `:8080` | listen address |
| `GPODDER_SYNC_DB` / `-db` | `/data/gpodder-sync.db` in the image | SQLite path |

`gpodder-sync user list` and `gpodder-sync user delete NAME` (deletes all of that user's data)
manage stored users. Credentials live in the proxy.

## API coverage

The endpoints AntennaPod uses (`/api/2/`):

| Endpoint | |
|---|---|
| `POST auth/{user}/login.json`, `logout.json` | confirms the proxy authenticated the user (no session cookie) |
| `GET devices/{user}.json`, `POST devices/{user}/{device}.json` | list / create / update devices |
| `GET`/`POST subscriptions/{user}/{device}.json` | subscription deltas with `since` |
| `GET`/`POST episodes/{user}.json` | episode actions with `since`, `podcast`, `device`, `aggregated` |

`GET /healthz` is open to any peer (it only returns `ok`). Not implemented: the simple API / OPML
export, sync groups, directory/search, settings, favourites, and gPodder desktop's unauthenticated mode.

## Development

Open the folder in VS Code → *Reopen in Container* (`.devcontainer/`). It's set up for rootless
podman (`--userns=keep-id`; remove that `runArgs` line for Docker, and set
`"dev.containers.dockerPath": "podman"` in VS Code for podman). The container has Go 1.27.3,
gopls, staticcheck, govulncheck, delve and sqlite3, and trusts localhost as the "proxy":

```sh
go run . serve &
curl -H 'Remote-User: alice' localhost:8080/api/2/devices/alice.json
go test -race ./... && staticcheck ./... && govulncheck ./...
```
