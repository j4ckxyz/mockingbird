# Client and API quirks

A running log of things that matter for 2009-2010 Twitter clients. Add to it as clients are tested.

## Where the reference data comes from

- apiwiki.twitter.com (2009 to mid-2010) examples were **hand-written** and contain typos (for example `<contributors_enabled>false</verified>`). They list fields but not reliably their order.
- dev.twitter.com (mid-2010 on) examples are **real API output**. Where the two disagree, the bridge follows dev.twitter.com: user fields end `... profile_background_tile, profile_use_background_image, notifications, geo_enabled, verified, following, statuses_count, lang, contributors_enabled, follow_request_sent`. The 2009 order was `... statuses_count, notifications, following, verified`.
- `rate_limit_status.xml` element order changed between the two: 2009 docs show `remaining-hits, hourly-limit, reset-time, reset-time-in-seconds`, while real 2010 output is `hourly-limit, reset-time-in-seconds, reset-time, remaining-hits`. The bridge uses the real order.
- Mid-2009 docs describe a pre-launch retweet shape, `<retweet_details>`. What shipped in November 2009 is `retweeted_status`, placed **before** `<user>` in XML.
- The Wayback CDX index for dev.twitter.com is full of junk URLs scraped from JavaScript. `scripts/fetch_wayback.py` filters them out. WebFetch cannot reach web.archive.org; curl can. The archive often refuses connections under load, so the script retries.

## IDs

- **Twitterrific for iPhone broke when status IDs passed 2^31** (the "Twitpocalypse", 12 June 2009, "YAJL error 3"); even 2.0.1 needed a fix. Twitterrific 1.x predates that fix, so assume it parses IDs as signed 32-bit integers. Bridge IDs are sequential per namespace from 1, so they stay below 2^31 for a very long time.
- IDs are assigned in **first-seen order**, oldest first within each batch. A page seen together sorts correctly by ID. An old post first seen later, for example when scrolling back or opening a user's profile, gets a *higher* ID than newer posts already seen. Clients that sort purely by ID may show it out of place. The bridge never compares IDs itself: `since_id` and `max_id` are resolved to timestamps.
- Clients page back with `max_id = oldest_id - 1`. Because bridge IDs aren't contiguous per timeline, `oldest - 1` usually belongs to an unrelated status. The bridge remembers the last ID it served on each feed (per viewer) and treats `max_id + 1 == last served` as "continue from there".
- Upserts that hit a conflict still consume an SQLite AUTOINCREMENT value, so ID requests are de-duplicated within a batch before insert.
- Retweets get their own ID (the repost record URI), as Twitter's retweets did. Destroying that ID undoes the repost.
- Search API user IDs differed from REST user IDs on real Twitter (Issue 214). The bridge uses one user ID space everywhere, which is strictly more compatible.

## Text

- Twitter's `text` was "escaped and HTML encoded": `<`, `>` and `&` arrive as entities in both JSON and XML (XML then escapes the `&` again). Quotes were **not** escaped in element text. Some clients echo the entities back when quoting or retweeting, so outgoing text is unescaped first.
- The Search API's `source` is entity-encoded (`&lt;a href=&quot;...`); the REST API's `source` is raw HTML.
- Bluesky shortens long links in the visible text (`example.com/very/lo...`) and keeps the target in a facet. Old clients only see text, so link facets are expanded to the full URL.
- Images, videos and quote posts become links in the text: to `/p/<id>` (a no-JavaScript page) for media, and to the quoted post's `/p/<id>`. Link cards append their URL if it isn't already in the text.
- XML 1.0 forbids most control characters. One post containing U+0007 would make an entire timeline unparseable on the device, so the XML writer drops them.
- Old clients count down from 140 and may refuse to send longer posts. The bridge accepts up to 300 graphemes (Bluesky's limit) and returns Twitter's 403 "Status is over 300 characters." beyond that.
- Clients linkify `@word` and stop at the first dot, so tapping `@alice.bsky.social` opens `users/show/alice`. Bare names resolve to handles the viewer has seen recently, then to `<name>.bsky.social` (`MB_DEFAULT_HANDLE_HOST`). The same rule applies to outgoing `@mentions` and to the login username.

## Timelines

- `friends_timeline` on real Twitter excluded retweets "for backwards compatibility". The bridge includes reposts there too (with `RT @handle:` fallback text), because clients old enough to use only `friends_timeline` would otherwise never see them.
- `friends/ids` and `followers/ids` in 2010 returned the cursored object (`ids`, `next_cursor`, `previous_cursor`) when `cursor` was given, and a bare array otherwise. Both are supported.
- `help/test` returned the JSON string `"ok"` and XML `<ok>true</ok>`.
- `account/end_session` answered 200 with an error-shaped body `{"request":..., "error":"Logged out."}`.
- Bluesky only shows an account's likes to that account, so `favorites/<other user>` is always empty.
- `statuses/friends` and `statuses/followers` omit each user's latest `status`: fetching one per user would multiply upstream calls. Watch whether any client depends on it.

## Auth

- Tweetie 2.0 uses Basic Auth. xAuth (`x_auth_mode=client_auth`) arrived in early 2010, and later Tweetie builds are reported to use it; confirm on the emulator. xAuth access tokens look like `<user_id>-<random>`, as Twitter's did; some clients parse the user ID out of the token.
- Twitter used `WWW-Authenticate: Basic realm="Twitter API"` on 401s.
- App password sessions carry scope `com.atproto.appPass`, or `com.atproto.appPassPrivileged` when DMs are allowed. Full passwords carry `com.atproto.access`; those sessions are revoked immediately and the login is refused. An `AuthFactorTokenRequired` error also means a main password was used, because only main passwords trigger email 2FA.

## Transport

- iPhone OS 3 does not send SNI, so the legacy TLS listener serves one certificate carrying every hostname.
- Go 1.27 removed the `tlsrsakex`, `tls3des` and `tls10server` GODEBUG knobs. TLS 1.0 and `TLS_RSA_WITH_AES_128_CBC_SHA` still work when set explicitly in `tls.Config`. Verified with a Go client and with `openssl s_client -tls1 -cipher 'AES128-SHA@SECLEVEL=0'`. Modern OpenSSL refuses TLS 1.0 unless `@SECLEVEL=0` is given.
- Name constraints on the bridge CA are marked **non-critical**. Verifiers that understand them enforce them; ones that don't still accept the certificate. It is untested whether iPhone OS 3 enforces them.
- iPhone OS networking sends `Accept-Encoding: gzip`. Compressing responses is the single biggest bandwidth saving (about 1.2 KB per poll in the load test).

## Upstream

- indigo's generated lexicon types fail a whole response when any record has an unregistered `$type`, for example a quote of a third-party record. The bridge uses lenient hand-written structs instead.
- The public AppView refuses anonymous `searchPosts`. Anonymous Search API requests get empty results with a `warning`; clients that send credentials get real results.
- `getActorLikes` works only for the viewer.
- Chat calls go through the user's PDS with `atproto-proxy: did:web:api.bsky.chat#bsky_chat`, and fail with "Bad token scope" unless the app password allows DMs.

## To verify on the emulator

- Whether Tweetie 2.0 insists on HTTPS for any call. If it does, enable the legacy TLS listener and install the CA.
- Whether Tweetie or Twitterrific sort timelines by ID or by date. This decides how visible the first-seen ID ordering is.
- Whether either client needs `status` inside users from `statuses/friends`.
- What either client does with posts over 140 characters.
- Whether iPhone OS 3 accepts the name-constrained CA. If not, set `MB_TLS_NAME_CONSTRAINTS=false` and delete `/data/tls` to reissue. If it rejects SHA-256 signatures, also set `MB_TLS_SHA1=true`.
