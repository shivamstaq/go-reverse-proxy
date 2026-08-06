# Reverse Proxy — Go

A TLS-terminating reverse proxy built on Echo. It accepts HTTPS from a client,
forwards the request to an upstream over TLS, buffers the response, replaces a
configurable keyword in textual bodies, and returns the modified response.

Both hops are TLS. The upstream's certificate is verified against a configured
CA; there is no `InsecureSkipVerify` anywhere in the code.

## Mechanism

```
REQUEST                                     internal/proxy
  client ──HTTPS──▶ terminate TLS (proxy.crt)
                          │
                          ├─ CONNECT ──────────────────────────▶ 501 not implemented
                          │
                          ├─ Host:             → upstream authority
                          │  X-Forwarded-Host: → client's Host (if not already set)
                          │
                          └──HTTPS──▶ upstream        chain verified against ca.crt


RESPONSE                                    internal/rewriter  (ModifyResponse)
  upstream response
        │
        ├─ 1xx · 204 · 304 · 206 · Content-Range · empty ─────▶ forward unchanged
        ├─ Content-Encoding br · deflate · zstd · "gzip, br" ─▶ forward unchanged
        ├─ text/event-stream ────────────────────────────────▶ forward, still streaming
        ├─ image · video · audio · font · archive · unknown ─▶ forward unchanged
        ├─ charset not utf-8 / us-ascii ─────────────────────▶ forward unchanged
        ├─ HEAD ─────────────────────────────────────────────▶ forward, drop Content-Length
        │
        ▼  textual and decodable
   read whole body ──▶ gunzip if gzip ──▶ no keyword? ──────▶ restore original bytes
                            (≤100 MiB)         │
                                               ▼ replace every occurrence
                                          re-gzip if it was gzipped
                                               │
                                               ▼  set Content-Length
                                                  drop ETag · Content-MD5 · Digest
        ┌──────────────────────────────────────┘
        ▼
      client
```

`internal/rewriter` imports no Echo: it takes an `*http.Response` and nothing
else, so the rules above are testable without a server.

## Layout

| Path | Responsibility |
| --- | --- |
| `cmd/proxy` | Entry point: load config, build handler, serve, shut down |
| `internal/proxy` | TLS termination, upstream transport, request forwarding |
| `internal/rewriter` | Inspects responses, replaces the keyword, rebuilds body and headers |
| `internal/config` | The only reader of the environment; validates before anything starts |
| `internal/fixtures` · `internal/testca` · `cmd/upstream` | Test scaffolding: origin server and throwaway certificate chains |

## Commands

```sh
docker compose up -d --build     # certgen → upstream → proxy
./scripts/e2e.sh                 # 55 checks against the running stack
go test ./...                    # unit and integration tests
docker compose down -v           # stop; -v forces a new CA next time
```

```sh
docker compose cp proxy:/certs/ca.crt /tmp/ca.crt

curl --cacert /tmp/ca.crt https://localhost:8443/html          # keyword replaced
curl --cacert /tmp/ca.crt https://localhost:8443/image         # binary, byte-identical
curl --cacert /tmp/ca.crt --compressed https://localhost:8443/gzip
curl --cacert /tmp/ca.crt https://localhost:8443/echo          # what the upstream saw
curl --cacert /tmp/ca.crt -N https://localhost:8443/events     # streams, not buffered
```

Only the proxy publishes a port; the upstream is reachable solely from inside the
Compose network. `certgen` is a one-shot job that issues the CA and both leaf
certificates into a shared volume. It is idempotent — Compose does not recreate a
running service when it re-runs, so reissuing every time would leave the upstream
presenting a leaf signed by the previous CA.

Without Docker:

```sh
./scripts/certgen.sh ./certs
go run ./cmd/upstream &
KEYWORD=test-keyword REPLACEMENT='[REDACTED]' go run ./cmd/proxy
```

## Configuration

`proxy.env` and `upstream.env` supply the deployment's values through Compose's
`env_file`. The Dockerfiles set no `ENV`, so each key is defined in one place.

| Variable | Default | Notes |
| --- | --- | --- |
| `KEYWORD` | — | **Required.** No default: a silent fallback would make a typo look like a bug |
| `REPLACEMENT` | — | **Required.** Replacement is literal and case-sensitive |
| `LISTEN_ADDR` | `:8443` | Client-facing address |
| `UPSTREAM_URL` | `https://localhost:9443` | Must be `https` |
| `CA_CERT_FILE` | `certs/ca.crt` | CA that signed the upstream certificate |
| `TLS_CERT_FILE` · `TLS_KEY_FILE` | `certs/proxy.*` | Keypair presented to clients |
| `LOG_LEVEL` | `info` | `debug` logs the rewriter's decision per response |
