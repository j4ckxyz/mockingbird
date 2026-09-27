# For developers

```sh
go test ./...                                   # unit, end-to-end and golden tests
go test -race ./...
go test ./internal/api -run Golden -update      # refresh output snapshots after an intentional change
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

- **Golden tests** compare the output with 68 real Twitter API responses from 2010 (`testdata/reference`, with sources listed in `SOURCES.md`). Every field must exist with the right type, and XML elements must appear in the right order. mockingbird's exact output is also saved in `testdata/golden`. To refresh the references, run `python3 scripts/fetch_wayback.py && python3 scripts/extract_refs.py`.
- **Live tests** run against a real Bluesky account when you provide one:
  ```sh
  MB_TEST_HANDLE=you.bsky.social MB_TEST_APP_PASSWORD=xxxx-xxxx-xxxx-xxxx go test ./internal/integration -v
  ```
  Add `MB_TEST_ALLOW_WRITES=1` to also post, like, reply and delete, or `MB_TEST_DM_APP_PASSWORD` to test messages.
- **Load test:** `mockingbird loadtest -users 3000 -rps 100 -duration 2m` simulates thousands of old apps polling a pretend Bluesky server. It reports the delay mockingbird adds, memory use and data sent. It also works inside the image: `docker run --rm mockingbird:local loadtest`.

**Code layout**

| Directory | Contents |
|---|---|
| `cmd/mockingbird` | The program: server, `healthcheck`, `genkeys`, `loadtest` |
| `internal/api` | The Twitter API: routing, sign-in, every endpoint |
| `internal/translate` | Bluesky to Twitter conversion, and links, mentions and hashtags for new posts |
| `internal/twitter` | Twitter's JSON, XML, RSS and Atom formats |
| `internal/session`, `internal/ident` | Bluesky sign-in, sessions and handle lookups |
| `internal/netguard` | The outgoing-connection guard |
| `internal/media` | Image resizing, signed image links and the image cache |
| `internal/web` | The landing page, sign-in form and post pages |
| `internal/tlslegacy` | The old-style HTTPS listener and its certificate authority |
| `internal/store` | The SQLite database |
| `deploy/pi` | Raspberry Pi and Cloudflare Tunnel deployment with the network sandbox |
