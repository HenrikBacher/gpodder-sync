# gpodder-sync

A small, self-hosted podcast sync server for [AntennaPod](https://antennapod.org/), speaking the
gpodder.net v2 API. Single static Go binary, SQLite storage, ~15 MB distroless image, built for Unraid.

**It holds no passwords.** A reverse proxy (Caddy `basic_auth`, or Caddy + Authelia) checks
AntennaPod's basic-auth login and forwards the username in a header. Like gpodder.net, the server
then issues a session cookie, which AntennaPod uses for the rest of the sync without resending the
password. Every connection that doesn't come from the trusted proxy IP is refused.

## Features

- Subscriptions and episode actions (play position, downloads, deletes) synced across devices
- Subscriptions are shared by all of a user's devices (no gpodder.net "sync groups" needed)
- Users are created automatically the first time the proxy forwards a request for them
- Runs as `nobody:users` (99:100), read-only root filesystem friendly, built-in health check

## How requests are trusted

1. The peer address must be in `GPODDER_SYNC_TRUSTED_PROXIES`, otherwise `403`.
2. If the proxy's user header (`Remote-User` by default) is set, it must match the username in the
   URL. Otherwise, the `sessionid` cookie from `login.json` must belong to that user. If neither
   holds, the server returns `401`.

The proxy checks credentials on any request that has an `Authorization` header, and passes the
other requests through for the server to check the cookie. Sessions expire after 30 days without
use and end on logout. Only a hash of each session token is stored.

So the proxy **must** strip any client-supplied `Remote-User` header, and nothing but the proxy
may be able to reach the container:

- Don't publish a port. Put the container on the same custom Docker network as Caddy, and only there.
- Give Caddy a fixed IP on that network and trust **that IP only**, never the subnet: any other
  container on a trusted subnet could send the header, and published-port traffic can appear to
  come from the network gateway.

## Unraid

The image is published to `ghcr.io/henrikbacher/gpodder-sync` (amd64 + arm64, public, no login
needed). `:latest` follows `main`; `:X.Y.Z` / `:X.Y` are pinned releases.

### 1. Make appdata an exclusive share

SQLite's WAL mode isn't safe on Unraid's FUSE layer (`shfs`), which `/mnt/user` normally goes
through. Make appdata exclusive so `/mnt/user/appdata` points straight at the pool (Unraid 6.12+):

1. Settings → Global Share Settings → *Permit exclusive shares*: Yes (with the array stopped).
2. Shares → appdata: Primary storage = your pool, Secondary storage = None, NFS export off.
3. Move any appdata files off the array disks, so `ls -d /mnt/disk*/appdata` finds nothing.

Check with `stat -f -c %T /mnt/user/appdata/`: `btrfs`, `xfs` or `zfs` is good, `fuse` is not.
It silently stops being exclusive if appdata gets files on another disk again, so re-check after
changing share settings, and recreate the container after enabling it.

### 2. Put Caddy on a custom network with a fixed IP

The template expects a network called `proxynet`. Create it once from the Unraid terminal if you
don't have one:

```sh
docker network create --subnet 172.20.0.0/24 proxynet
```

Edit the Caddy container: **Network Type** = `proxynet`, **Fixed IP address** = e.g. `172.20.0.2`.
(Settings → Docker → *Preserve user defined networks*: Yes, so the network survives a Docker restart.)

### 3. Install the template

```sh
wget -O /boot/config/plugins/dockerMan/templates-user/my-gpodder-sync.xml \
  https://raw.githubusercontent.com/HenrikBacher/gpodder-sync/main/unraid/gpodder-sync.xml
```

### 4. Add the container

Docker → **Add Container** → pick **gpodder-sync** from the template list, then set:

- **Trusted proxy IP**: Caddy's exact IP (`172.20.0.2`), never the subnet.
- **Network**: `proxynet` (preset). Leave it with no port mapping; only Caddy should reach it.

The template also:

- maps `/mnt/user/appdata/gpodder-sync` → `/data`. The image runs as 99:100, which already owns
  appdata, so no `chown` is needed.
- adds hardening Extra Parameters: `--read-only --tmpfs /tmp --cap-drop=ALL --security-opt=no-new-privileges`.
- gets Unraid's `TZ`, which is used for log timestamps.

### 5. Configure Caddy and AntennaPod

Add a site block from [Proxy setup A](#proxy-setup-a-caddy-basic_auth) or
[B](#proxy-setup-b-caddy--authelia), reload Caddy, then connect [AntennaPod](#antennapod).

### 6. Check it works

- The container shows **healthy** in the Docker tab.
- `curl -u alice:PASSWORD https://podsync.example.com/api/2/devices/alice.json` returns `[]`
  (or your devices), not `401`/`403`.
- After AntennaPod's first sync, `docker exec gpodder-sync /gpodder-sync user list` shows the user.

The image has no shell (distroless), so Unraid's *Console* button won't work; use `docker exec`
as above. Updates: Docker tab → *Check for Updates* pulls the newest `:latest`.

### Building the image yourself

```sh
docker build --platform linux/amd64 --build-arg VERSION=0.1.0 -t gpodder-sync:latest .
docker save gpodder-sync:latest | ssh root@unraid docker load
```

Then set the container's **Repository** to `gpodder-sync:latest`.

## Proxy setup A: Caddy `basic_auth`

Hash each password (argon2id needs **Caddy 2.11+**):

```sh
docker run --rm caddy:2.11.7 caddy hash-password -a argon2id
```

```caddyfile
podsync.example.com {
	route {                                  # keeps the directives in this order
		request_header -Remote-User          # drop client-supplied values first
		@credentials header Authorization *
		basic_auth @credentials argon2id {
			alice $argon2id$v=19$m=47104,t=1,p=1$...
		}
		request_header @credentials Remote-User {http.auth.user.id}
		reverse_proxy gpodder-sync:8080
	}
}
```

Requests without an `Authorization` header reach the server with no user, and need a valid session
cookie. Set `Remote-User` only under `@credentials` as shown: on other requests Caddy would send
the literal text `{http.auth.user.id}`. Caddy doesn't rate-limit failed logins by itself; add the CrowdSec bouncer or fail2ban on
Caddy's logs.

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
		@credentials header Authorization *
		forward_auth @credentials authelia:9091 {
			uri /api/authz/forward-auth
			copy_headers Remote-User
		}
		reverse_proxy gpodder-sync:8080
	}
}
```

Authelia only sees requests with credentials (the login); the server checks the session cookie on
the rest. Use a dedicated Authelia user (only in group `podsync`) for AntennaPod rather than your main login.
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
| `POST auth/{user}/login.json`, `logout.json` | proxy checks credentials; login sets the `sessionid` cookie, logout ends it |
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
