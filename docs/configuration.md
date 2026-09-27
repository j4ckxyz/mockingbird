# Settings reference

All settings are environment variables in `.env`. Anything secret can instead be read from a file by adding `_FILE` to the name, for example `MB_SECRET_KEY_FILE=/run/secrets/key`.

**Required**

| Setting | Meaning |
|---|---|
| `MB_SECRET_KEY` | 32 random bytes (hex or base64). Used to sign sessions, tokens and image links. `mockingbird genkeys` makes one. |
| `MB_ENCRYPTION_KEY` | 32 different random bytes. Used to encrypt sign-in tokens at rest. |

**Common**

| Setting | Default | Meaning |
|---|---|---|
| `MB_PUBLIC_URL` | `http://localhost:8080` | The address people use; appears in image and post links. |
| `MB_HTTP_ADDR` | `:8080` | Where to listen for plain HTTP. |
| `MB_ADMIN_ADDR` | `127.0.0.1:9090` | Where to serve metrics. Must be the machine itself or a Tailscale address. |
| `MB_LEGACY_TLS_ADDR` | off | Turn on the old-style HTTPS listener, for example `:443`. |
| `MB_DEFAULT_HANDLE_HOST` | `bsky.social` | Added to names typed without a dot. |
| `MB_TRUSTED_PROXIES` | none | Addresses of proxies whose `X-Forwarded-For` header is trusted. |
| `MB_LOG_LEVEL` | `info` | Set to `debug` to log every request. |
| `MB_DATA_DIR` | `/data` | Where the database and caches live. |

**Advanced**

| Setting | Default | Meaning |
|---|---|---|
| `MB_BRIDGE_HOSTS` | host of `MB_PUBLIC_URL` | The bridge's own hostnames (used on HTTPS certificates). |
| `MB_SEARCH_HOSTS` | `search.twitter.com` | Hosts treated as the search server. |
| `MB_IMAGE_CACHE_MB` | `512` | Disk space for resized images. |
| `MB_RESPONSE_CACHE_TTL` | `20s` | How long a user's repeated identical requests are served from cache. |
| `MB_PROFILE_CACHE_TTL` | `5m` | How long follower counts are cached. |
| `MB_IDENTITY_CACHE_TTL` | `6h` | How long handle lookups are cached. |
| `MB_MAX_UPSTREAM_PAGES` | `6` | How far back to look when an app asks for older posts. |
| `MB_PER_IP_RATE` / `MB_PER_IP_BURST` | `5` / `60` | Requests per second (and burst) allowed per internet address. |
| `MB_PER_ACCOUNT_RATE` / `MB_PER_ACCOUNT_BURST` | `1` / `40` | The same, per Bluesky account. |
| `MB_REPORTED_RATE_LIMIT` | `350` | The hourly limit reported to apps (kept generous so they never slow themselves down). |
| `MB_OAUTH_CONSUMERS` | none | `key:secret` pairs for apps whose signatures should be checked. These apps may also redirect to a website after sign-in without the confirmation step. |
| `MB_OAUTH_TIMESTAMP_WINDOW` | `1h` | Allowed clock difference for signed requests. |
| `MB_SESSION_IDLE_EXPIRY` | `1440h` | Unused sign-ins are forgotten after this (60 days). |
| `MB_TLS_EXTRA_HOSTS` | the Twitter hostnames | Extra names on the HTTPS certificate. |
| `MB_TLS_COMMON_NAME` | host of `MB_PUBLIC_URL` | Main name on the HTTPS certificate. |
| `MB_TLS_NAME_CONSTRAINTS` | `true` | Limit the certificate authority to those names. |
| `MB_TLS_SHA1` | `false` | Use SHA-1 signatures, for very old devices that reject SHA-256. |
| `MB_ADMIN_IN_CONTAINER` / `MB_ADMIN_TRUSTED_PEERS` | off / none | Let the metrics listener work inside a Docker network (see `deploy/pi`). |
| `MB_PUBLIC_SEARCH_URL` | `https://api.bsky.app` | Used for searches from apps that don't sign in (old Twitter search needed no login). |
| `MB_PLC_URL`, `MB_APPVIEW_PROXY`, `MB_CHAT_PROXY`, `MB_PUBLIC_APPVIEW_URL`, `MB_IMAGE_CDN_URL`, `MB_PUBLIC_TIMELINE_FEED` | Bluesky's | Only change these to use different Bluesky infrastructure. |
| `MB_INSECURE_ALLOW_PRIVATE_NETWORKS` | `false` | For development only. Turns off the protection described in [security.md](security.md). Never use it on a public server. |

If you change `MB_TLS_NAME_CONSTRAINTS` or `MB_TLS_SHA1`, delete `/data/tls` so the certificate authority is recreated, and reinstall the profile on your devices.

## Apps that insist on HTTPS

A few apps only use `https://`. mockingbird can run an extra, deliberately old-fashioned HTTPS listener (TLS 1.0) that 2009 iPhones understand, using its own certificate authority. This only makes sense on your home network (Option B), since Cloudflare handles HTTPS for Option A.

1. Set `MB_LEGACY_TLS_ADDR=:443` in `.env` and run `docker compose up -d`.
2. On the old iPhone, open `http://<your bridge>/` in Safari, tap **Install profile**, and follow the prompts.

> **Think before installing this.** The device will trust certificates made by your bridge. The certificate authority is limited to the bridge's own names and the Twitter names, but very old iPhones may not enforce that limit. Anyone who stole the key from your bridge's data folder could pretend to be those sites to that device. Only install it on a device you use for vintage apps, and remove it afterwards under **Settings › General › Profiles**.
