# mockingbird

A self-hosted bridge that lets vintage Twitter clients use Bluesky. That means iPhone OS 2 and 3 era apps such as Tweetie 2.0 and Twitterrific 1.x, and anything that lets you set a custom API root.

mockingbird serves the 2009-2010 **Twitter REST API v1** and **Search API** in JSON, XML, JSONP, RSS and Atom. It translates every request into AT Protocol calls against the user's own PDS, proxied to the Bluesky AppView and chat service.

It is a single static Go binary. It runs in Docker on a Raspberry Pi 5 (arm64) and is built for a home internet connection: it caches aggressively, gzips everything, and resizes images to what an original iPhone can display.

> **Security warning.** Vintage clients send credentials over plain HTTP, or at best TLS 1.0, so anyone on the network path can read them. mockingbird therefore **accepts only Bluesky app passwords** and refuses main account passwords. An app password can be revoked at any time from Bluesky's settings without affecting the account.

## Contents

- [Quick start](#quick-start)
- [Client setup](#client-setup)
- [What maps to what](#what-maps-to-what)
- [Security model](#security-model)
- [Configuration](#configuration)
- [Operations](#operations)
- [Development and testing](#development-and-testing)
- [Load test results](#load-test-results)
- [Limitations](#limitations)

## Quick start

On the Pi (or any Linux host with Docker):

```sh
git clone <this repo> mockingbird && cd mockingbird
cp .env.example .env
docker compose build
docker compose run --rm mockingbird genkeys >> .env   # appends two keys
$EDITOR .env        # set MB_PUBLIC_URL, remove the empty MB_SECRET_KEY= / MB_ENCRYPTION_KEY= lines
# Let the non-root container bind ports 80/443 (host networking), once:
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-mockingbird.conf && sudo sysctl --system
docker compose up -d
curl http://localhost/help/test.json    # "ok"
```

Open `http://<your host>/` for the setup page. For clients to reach it from outside, forward TCP 80 (and 443 if you use the legacy TLS listener) on your router to the Pi.

The compose file runs the container with host networking, a read-only root filesystem, all capabilities dropped, `no-new-privileges`, a non-root user (UID 65532) and one named volume for `/data` (database, image cache, TLS material).

### Behind a Cloudflare Tunnel (the Pi deployment)

`deploy/pi/` runs the bridge on its own Docker network with ports published on 127.0.0.1 only. A dedicated nftables table (`deploy/pi/firewall.nft`, loaded by `mockingbird-firewall.service` before Docker) lets that network reach the public internet only.

```sh
sudo install -d /etc/mockingbird
sudo install -m 0644 deploy/pi/firewall.nft /etc/mockingbird/
sudo install -m 0644 deploy/pi/mockingbird-firewall.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now mockingbird-firewall
cd deploy/pi && cp env.example .env   # then fill in the two keys; chmod 600 .env
docker compose up -d --build
```

Then add a public hostname to the existing tunnel in the Cloudflare dashboard (Networks › Tunnels › *tunnel* › Published application routes): `mockingbird.j4ck.xyz`, service `HTTP`, URL `localhost:18480`. Leave "Always Use HTTPS" off for that hostname, and make sure no bot challenge applies to it, because vintage clients speak plain HTTP and cannot solve challenges. `MB_TRUSTED_PROXIES` in the compose file makes the bridge use cloudflared's `X-Forwarded-For`, so rate limits apply per real client IP.

## Client setup

Every client needs a Bluesky **app password**, created at bsky.app › Settings › Privacy and security › App passwords. Tick *Allow access to your direct messages* if you want DMs. The username is your handle (for example `alice.bsky.social`). A bare name like `alice` expands to `alice.bsky.social` (see `MB_DEFAULT_HANDLE_HOST`). DIDs work too.

### Clients with a configurable API root

| Setting | Value |
|---|---|
| REST API root | `http://your-host/` or `http://your-host/1/` |
| Search API root | `http://your-host/` (`/search.json`, `/search.atom`, `/trends.json`) |
| OAuth | `http://your-host/oauth/request_token`, `/oauth/authorize`, `/oauth/access_token` |
| xAuth | `POST http://your-host/oauth/access_token` with `x_auth_mode=client_auth` |
| TwitPic-style image service | `http://your-host/api/upload` and `/api/uploadAndPost` |

### Tweetie 2.0 and Twitterrific 1.x (hardcoded hosts)

These clients always call `twitter.com`, `api.twitter.com` and `search.twitter.com` over plain HTTP. Point those names at the bridge. The bridge routes by path and `Host` header, so no other change is needed.

- **iPhone Simulator (iPhone OS 3):** the simulator uses the Mac's resolver, so add this to `/etc/hosts` on the Mac:
  ```
  192.0.2.10  twitter.com www.twitter.com api.twitter.com search.twitter.com
  ```
  Replace `192.0.2.10` with your bridge's address.
- **Real device:** run a DNS server you control (Pi-hole, dnsmasq or your router) that answers those four names with the bridge's address, and set it as the device's DNS server under Settings › Wi-Fi. With dnsmasq:
  ```
  address=/twitter.com/192.0.2.10
  address=/search.twitter.com/192.0.2.10
  ```

Tweetie 2.0 signs in with Basic Auth. Twitterrific 1.x uses Basic Auth and the XML endpoints.

### Clients that insist on HTTPS

Set `MB_LEGACY_TLS_ADDR=:443`. The bridge then runs a **separate** HTTPS listener for TLS 1.0 to 1.2 with RSA key exchange and AES-CBC suites (for example `TLS_RSA_WITH_AES_128_CBC_SHA`). It uses a certificate issued by a bridge-specific CA and covering your hostname plus the Twitter hostnames. On the device, open `http://your-host/` in Mobile Safari and install **`/mockingbird.mobileconfig`** (or `/ca.crt`). The setup page shows the CA's SHA-256 fingerprint so you can compare it.

> **Installing the CA is a real trust decision.** The device will trust certificates the bridge's CA issues. By default the CA is name-constrained to the bridge and Twitter hostnames, but it is not known whether iPhone OS 3 enforces name constraints. Anyone who obtains `/data/tls/ca.key` could impersonate those sites, and possibly others, to that device. Only install it on a device you use for vintage apps, keep the data volume private, and remove the profile when you are done (Settings › General › Profiles).

## What maps to what

| Twitter endpoint | Bluesky |
|---|---|
| `statuses/home_timeline`, `friends_timeline` | `app.bsky.feed.getTimeline` (reposts become retweets) |
| `statuses/user_timeline` | `app.bsky.feed.getAuthorFeed` |
| `statuses/mentions`, `replies` | `listNotifications` (mention, reply, quote), hydrated with `getPosts` |
| `statuses/show/:id` | `getPosts` |
| `statuses/update` | `createRecord` `app.bsky.feed.post`, with facets for mentions, links and hashtags; reply root and parent refs taken from the parent post |
| `statuses/destroy/:id` | `deleteRecord` (post, or the repost when the ID is a retweet) |
| `statuses/retweet/:id`, `retweets/:id`, `retweets_of_me`, `retweeted_by_me`, `retweeted_to_me` | repost records, `getRepostedBy`, repost notifications |
| `favorites`, `favorites/create`, `favorites/destroy` | `getActorLikes`, like records |
| `account/verify_credentials`, `users/show`, `users/lookup`, `users/search` | `getProfile(s)`, `searchActors` |
| `friendships/create`, `destroy`, `show`, `exists`, `friends/ids`, `followers/ids`, `statuses/friends`, `statuses/followers` | follow records, `getRelationships`, `getFollows`, `getFollowers` |
| `blocks/*`, `report_spam` | block records (report_spam blocks) |
| `direct_messages`, `sent`, `new`, `destroy` | `chat.bsky.convo.*` through the PDS (needs a DM-enabled app password) |
| `search` (search host), `trends`, `trends/current`, `daily`, `weekly`, `trends/1` | `searchPosts`, `getTrendingTopics` |
| `saved_searches/*` | stored locally in SQLite |
| `account/rate_limit_status`, `help/test`, `end_session` | local |
| anything else | Twitter-style 404, logged once with its parameter names |

**Translation details**

- **IDs:** numeric IDs are sequential per namespace (statuses, users, DMs), stored in SQLite, and kept below 2^31 for 32-bit clients. Responses also include `id_str`.
- **Paging:** `since_id` and `max_id` are resolved to timestamps and the Bluesky cursor is walked to that point, rather than comparing IDs. `count`, `rpp`, `page` and JSONP `callback` are honoured, as is `suppress_response_codes`.
- **Text out:** shortened link facets are expanded to full URLs. Images and videos become a link to `/p/<id>`, a no-JavaScript page for in-app browsers. Quote posts link to the quoted post's page. `<`, `>` and `&` are escaped as Twitter did. Dates use `Wed Aug 27 13:08:45 +0000 2008` (REST) or `Wed, 27 Aug 2008 13:08:45 +0000` (Search).
- **Retweets:** `retweeted_status` plus `RT @handle: text` fallback text for clients that ignore it.
- **Images:** avatars go through the bridge at Twitter's sizes (`_mini` 24px, `_normal` 48px, `_bigger` 73px); everything else is capped at 480px wide. URLs are HMAC-signed over the image identity only, so clients can still swap the size suffix themselves. Images are cached on disk with an LRU size cap and immutable cache headers.
- **Uploads:** a TwitPic-compatible `/api/upload` stores the image as a blob on your PDS and returns a bridge URL. A later status containing that URL gets the image attached as a real embed, and the URL is removed from the text.

See [NOTES.md](NOTES.md) for the quirks behind these choices.

## Security model

- **App passwords only.** After `createSession`, the access token's scope must be `com.atproto.appPass` or `com.atproto.appPassPrivileged`. Anything else (a main password) has its session revoked upstream immediately and the login is refused with a clear message.
- **No password storage.** Basic Auth sessions are keyed by HMAC-SHA256 (server secret) of handle and password. The password exists in memory only while the request is handled and is never logged or written. `createSession` runs only on a cache miss; sessions survive restarts.
- **Tokens at rest.** Access and refresh tokens are sealed with AES-256-GCM (`MB_ENCRYPTION_KEY`), bound to their row. Tokens refresh automatically, one refresh per session at a time. If a session is revoked upstream, the client gets a Twitter-style 401 (Basic Auth clients log in again transparently).
- **Brute-force limits.** Per IP: 10 failed logins, then a lockout starting at 1 minute and doubling up to 1 hour. Per account: 5 failures, then 2 minutes doubling up to 1 hour. A correct password with an existing cached session is never locked out by someone else's failures.
- **OAuth trade-off.** Twitter clients embed their own consumer key and secret, which the bridge cannot know. So the bridge's OAuth access token (`<user_id>-` followed by 256 random bits) is a **bearer credential**: whoever holds it can use the account until it is revoked, whatever signature they send. Only an HMAC of the token is stored. If you know a client's consumer secret, add it to `MB_OAUTH_CONSUMERS`. Requests from that consumer must then carry a valid HMAC-SHA1 (or PLAINTEXT) signature, a timestamp inside `MB_OAUTH_TIMESTAMP_WINDOW` and an unused nonce. The implementation reproduces the RFC 5849 and Twitter documentation test vectors. The sign-in page at `/oauth/authorize` is a plain form (no JavaScript, framing denied).
- **SSRF protection.** Handles, `did:web` documents and PDS endpoints are chosen by whoever logs in, and they are fetched from inside your network. Every outbound connection goes through a dialer that checks the actual IP being connected to, at connect time. It blocks loopback, RFC 1918, link-local, CGNAT and Tailscale (100.64.0.0/10), ULA, multicast, unspecified, NAT64, 6to4 and Teredo ranges. It also enforces HTTPS (except a configured plain-HTTP PLC mirror), caps redirects, bodies and time, and ignores proxy environment variables. Tests cover DNS rebinding, mixed DNS answers, IPv4-mapped IPv6 and redirects to internal hosts.
- **Limits.** Per-IP and per-account token buckets on every endpoint. Request bodies are capped at 64 KB (5 MB for uploads). Timeouts apply on every hop.
- **Output safety.** XML is escaped and stripped of characters XML 1.0 forbids. JSONP callback names are validated and the response is prefixed with `/**/`. HTML pages use `html/template` with a strict CSP. Only parameterised SQL is used, and secrets are compared in constant time.
- **Logging.** Authorization headers, passwords, tokens and request bodies are never logged. A redacting log handler scrubs anything credential-shaped as a backstop, including in panic traces. Unknown endpoints are logged with parameter names only.
- **Admin isolation.** Metrics (`/metrics`), pprof and the unknown-endpoint report (`/admin/unknown`) are served only on `MB_ADMIN_ADDR`. Startup fails for any address that is not loopback or Tailscale, and every admin request's peer address is checked again.
- **Supply chain.** Go modules are pinned in `go.sum`. Base images and GitHub Actions are pinned by digest. CI runs `govulncheck`.

## Configuration

All configuration is by environment variable. Secrets can also be read from a file named by the same variable with `_FILE` appended.

| Variable | Default | Meaning |
|---|---|---|
| `MB_SECRET_KEY` | *(required)* | 32 bytes (hex or base64). HMAC key for session keys, OAuth tokens and image URL signatures. |
| `MB_ENCRYPTION_KEY` | *(required)* | 32 bytes, different from the above. AES-256-GCM key for tokens at rest. |
| `MB_PUBLIC_URL` | `http://localhost:8080` | Base URL used in generated links (images, post pages). Use `http://`. |
| `MB_HTTP_ADDR` | `:8080` | Plain HTTP listener. |
| `MB_LEGACY_TLS_ADDR` | *(off)* | Legacy TLS 1.0 listener, for example `:443`. |
| `MB_ADMIN_ADDR` | `127.0.0.1:9090` | Comma-separated admin listeners; loopback or Tailscale addresses only. Retries until the Tailscale address exists. |
| `MB_DATA_DIR` | `/data` | Database, image cache and TLS material. |
| `MB_BRIDGE_HOSTS` | host of `MB_PUBLIC_URL` | Hostnames of the bridge itself (TLS certificate names). |
| `MB_SEARCH_HOSTS` | `search.twitter.com` | Hosts whose `/` redirects to the setup page (the Search API is served on every host). |
| `MB_DEFAULT_HANDLE_HOST` | `bsky.social` | Suffix for dotless names at login and in mentions. |
| `MB_TRUSTED_PROXIES` | *(none)* | CIDRs whose `X-Forwarded-For` / `X-Forwarded-Proto` are trusted. |
| `MB_OAUTH_CONSUMERS` | *(none)* | `key:secret,...` for consumers whose signatures must be verified. |
| `MB_OAUTH_TIMESTAMP_WINDOW` | `1h` | Allowed clock skew for signed requests (`0` disables the timestamp and nonce checks). |
| `MB_IMAGE_CACHE_MB` | `512` | Disk cap for the image cache. |
| `MB_RESPONSE_CACHE_TTL` | `20s` | Per-user response cache TTL (writes invalidate it). |
| `MB_PROFILE_CACHE_TTL` | `5m` | Shared cache of profile counts. |
| `MB_IDENTITY_CACHE_TTL` | `6h` | Handle and DID resolution cache (failures are cached for 5 minutes). |
| `MB_MAX_UPSTREAM_PAGES` | `6` | Upstream pages walked per request when paging by `max_id`, `since_id` or `page`. |
| `MB_PER_IP_RATE` / `MB_PER_IP_BURST` | `5` / `60` | Requests per second and burst per client IP. |
| `MB_PER_ACCOUNT_RATE` / `MB_PER_ACCOUNT_BURST` | `1` / `40` | Requests per second and burst per account. |
| `MB_REPORTED_RATE_LIMIT` | `350` | `hourly_limit` reported to clients; the reported remaining count never drops below 90% of it. |
| `MB_SESSION_IDLE_EXPIRY` | `1440h` | Sessions unused this long are purged. |
| `MB_TLS_EXTRA_HOSTS` | Twitter hostnames | Extra names on the legacy TLS certificate. |
| `MB_TLS_COMMON_NAME` | host of `MB_PUBLIC_URL` | Certificate CN (old clients check CN rather than SANs). |
| `MB_TLS_NAME_CONSTRAINTS` | `true` | Name-constrain the CA to the certificate's hosts (non-critical). |
| `MB_TLS_SHA1` | `false` | Sign the CA and certificate with SHA-1 instead of SHA-256, for very old verifiers. |
| `MB_PLC_URL` | `https://plc.directory` | PLC directory. |
| `MB_APPVIEW_PROXY` / `MB_CHAT_PROXY` | Bluesky's | `atproto-proxy` targets. |
| `MB_PUBLIC_APPVIEW_URL` | `https://public.api.bsky.app` | Used for anonymous requests and post pages. |
| `MB_IMAGE_CDN_URL` | `https://cdn.bsky.app` | Image CDN. |
| `MB_PUBLIC_TIMELINE_FEED` | Bluesky Discover | Feed used for `statuses/public_timeline`. |
| `MB_LOG_LEVEL` | `info` | `debug` logs every request (path, parameter names, status, timings, User-Agent). |
| `MB_INSECURE_ALLOW_PRIVATE_NETWORKS` | `false` | Disables the SSRF address check. Development only; never on a public deployment. |

Changing the TLS hostnames or common name reissues the leaf certificate but keeps the CA. `MB_TLS_NAME_CONSTRAINTS` and `MB_TLS_SHA1` apply to the CA itself, so they take effect only after you delete `/data/tls`. That creates a new CA, which every device then has to reinstall.

## Operations

- **Metrics:** `curl http://127.0.0.1:9090/metrics` (or your Tailscale address). Useful series:
  - `mockingbird_translation_seconds`: latency excluding upstream waits
  - `mockingbird_request_seconds`
  - `mockingbird_upstream_seconds{nsid}`
  - `mockingbird_logins_total{outcome}`
  - `mockingbird_response_cache_total`
  - `mockingbird_image_requests_total`
  - `mockingbird_unknown_endpoint_total`
- **What are clients calling that isn't implemented?** `curl http://127.0.0.1:9090/admin/unknown`, or grep the logs for `"msg":"unimplemented"`. Each unknown endpoint or ignored parameter is logged once, with the client's User-Agent.
- **Profiling:** `go tool pprof http://127.0.0.1:9090/debug/pprof/heap`.
- **Health:** `GET /healthz` on the public listener; the container healthcheck runs `mockingbird healthcheck`.
- **Backups:** the `/data` volume. The database contains only ID maps, sealed tokens, saved searches and upload references; the image cache can be deleted at any time.
- **Key rotation:** changing `MB_ENCRYPTION_KEY` invalidates stored sessions (users log in again; Basic Auth clients do this automatically). Changing `MB_SECRET_KEY` also invalidates OAuth tokens and image URLs.

## Development and testing

```sh
go test ./...                    # unit, end-to-end (fake PDS) and golden tests
go test -race ./...
go test ./internal/api -run Golden -update    # refresh output snapshots after an intended change
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

- **Golden tests** compare the bridge's output with 68 responses from the archived 2010 Twitter documentation (`testdata/reference`, with provenance in `SOURCES.md`). Every reference key must exist with the same JSON type, and XML element order must match at every level. The bridge's exact bytes are also snapshotted in `testdata/golden`.
  - Refresh the references with `python3 scripts/fetch_wayback.py && python3 scripts/extract_refs.py`.
  - The 2009 apiwiki samples are in `testdata/archived`.
- **Integration tests** run against a real Bluesky account when configured:
  ```sh
  MB_TEST_HANDLE=you.bsky.social MB_TEST_APP_PASSWORD=xxxx-xxxx-xxxx-xxxx go test ./internal/integration -v
  MB_TEST_ALLOW_WRITES=1 ...        # also posts, likes, replies and deletes a test post
  MB_TEST_DM_APP_PASSWORD=...       # DM-enabled app password for the DM test
  ```
- **Load test:** `mockingbird loadtest -users 3000 -rps 100 -duration 2m` (also available in the image: `docker run --rm mockingbird:local loadtest ...`).
  - It seeds an in-memory fake PDS with the given number of accounts (with configurable latency) and logs each in once.
  - It then drives home timeline polls with `since_id`, mentions, XML timelines, `verify_credentials`, user timelines and avatars at a fixed rate.
  - It reports client latency, translation overhead measured inside the server, upstream calls per request, bytes on the wire and the bridge's heap.
  - It exits non-zero if p95 translation overhead exceeds 50 ms. Run it on the Pi for real numbers.

## Load test results

These results come from the Mac used for development, not the Pi. Each run used 60 ms simulated upstream latency per call, and all runs had 0 errors.

| Setup | Users | Rate | p95 translation overhead | Bridge heap |
|---|---|---|---|---|
| Apple silicon, 6 cores, native | 3000 | 200 req/s | 3.8 ms | ~60 MB |
| arm64 container limited to **1 CPU** | 3000 | 100 req/s | 4.6 ms | ~55 MB |

For scale, 3000 users polling two endpoints every 2.5 minutes is about 40 req/s. Responses averaged 0.9 to 1.2 KB on the wire with gzip, under 1 Mbit/s of upload at 100 req/s. Upstream calls averaged 0.8 per client request.

## Limitations

- **ID order.** IDs are assigned in first-seen order to stay 32-bit safe. An old post first seen late (for example when scrolling back) has a higher ID than newer posts, and clients that sort purely by ID may show it out of place.
- **Content that doesn't map:**
  - Group chats are skipped.
  - Videos are shown as links.
  - Lists, geo and the streaming API are not implemented (404, logged).
- **Likes:** Bluesky shows likes only to their owner, so other users' favourites are empty.
- **App passwords are deprecated upstream.** Bluesky recommends OAuth for new apps but still supports app passwords. If they are ever removed, the bridge would need a Bluesky OAuth sign-in flow completed from a modern browser.
- **No affiliation.** mockingbird is not affiliated with Twitter, X or Bluesky.
