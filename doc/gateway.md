# The Fleet Gateway

The **fleet gateway** lets a user expose their local fleet daemon (`fleetd`) to
the internet — its **MCP server** (for remote AI agents) and its **gRPC API**
(for full remote control) — without opening a single inbound port on their
machine.

This document explains how the pieces fit together and how a request travels
end‑to‑end for both the MCP and gRPC paths.

> For operator/user setup (flags, env vars, security model), see the
> [Remote MCP](../README.md#remote-mcp) section of the README. This document is
> the architecture deep‑dive.

---

## 1. The core idea: a reverse tunnel

`fleetd` runs on a user's laptop or workstation — typically behind NAT or a
firewall, with **no reachable inbound port**. A normal reverse proxy can't reach
it. So the gateway inverts the direction:

- **`fleetd` dials *out*** to the gateway and keeps one long‑lived connection
  open.
- The gateway **pushes inbound requests back *down* that connection** to
  `fleetd`.

The single outbound connection is multiplexed with **[yamux](https://github.com/hashicorp/yamux)**,
so many independent requests/streams ride over it concurrently. The gateway holds
no fleet state and never inspects the application payload — it terminates the
inbound HTTP/MCP and HTTP/2/gRPC and reverse‑proxies each request down a tunnel
stream to the daemon, which is the real server on the far end.

```
        Behind NAT / firewall                          Public internet
 ┌───────────────────────────────┐            ┌──────────────────────────┐
 │            fleetd             │  dials out  │       fleet gateway       │
 │  remote.Manager ──Register──────(gRPC bidi)─▶  grpc  (:50051) HTTP/2  │◀── remote `fleet`
 │  (yamux over one bidi stream) │             │   • registration + tunnel │
 │                               │◀──pushes streams down (yamux)──────────│
 │  loopback MCP  (127.0.0.1)    │             │  public (:443)  HTTP/1   │◀── MCP agents
 │  tunnel gRPC server           │             │                          │
 └───────────────────────────────┘            └──────────────────────────┘
```

fleetd opens ONE long‑lived gRPC bidi stream (the `Register` method) and runs yamux
over it; the gateway opens yamux sub‑streams back down that one HTTP/2 stream. There
is no separate TCP control port — everything is HTTP/HTTP‑2, so an L7 proxy can front
the whole gateway.

---

## 2. Component map

| Component | Package | Role |
|-----------|---------|------|
| **Shared tunnel protocol** | `internal/tunnel` | The register handshake (length‑prefixed JSON), the yamux session helpers, the per‑stream **tag** byte, and the gRPC‑stream→`net.Conn` adapter (`StreamConn` + raw codec). Imported by *both* ends; depends only on stdlib + yamux. |
| **fleetd tunnel client** | `internal/server/remote` | `Manager` dials the gateway, registers, and serves inbound streams; `serveTunnel` demuxes them to MCP or gRPC. |
| **fleetd MCP server** | `internal/server/mcp.go` | The loopback MCP server (`127.0.0.1:<port>`), bearer‑token gated. |
| **fleetd gRPC server (tunnel)** | `internal/server` | A second `grpc.Server` (same `FleetService`) behind a bearer‑token interceptor, fed by the demux. The local unix‑socket server stays auth‑less. |
| **The gateway** | `internal/gateway` | Two listeners — public MCP (HTTP/1.1) and native gRPC (HTTP/2). The gRPC server hosts the transparent control proxy AND the fleetd `Register` method (`register.go`); plus the session registry and the `/mcp` route. An **isolated module** — imports only `internal/tunnel`, the stdlib, and `google.golang.org/grpc` (no fleetd internals). |
| **Remote `fleet` client** | `internal/fleetclient` | `gatewayEndpoint` — an ordinary gRPC dial to the gateway's gRPC listener, routing by the `fleet-session` metadata header. |

---

## 3. Registration (over the gRPC tunnel stream)

When the user enables remote MCP, `fleetd`'s `remote.Manager` opens a long‑lived
gRPC **bidi** stream — the `Register` method (`tunnel.RegisterMethod`) on the
gateway's gRPC endpoint. Both ends wrap that stream as a `net.Conn`
(`tunnel.StreamConn`, a raw‑codec adapter), so the *exact same* handshake + yamux
machinery runs over it as would over a TCP conn — there is no separate control port:

```
 fleetd (remote.Manager)                         gateway (register.go)
      │                                                  │
      │  open gRPC bidi  Register stream ─────────────────▶  handler runs
      │  (TLS for https / h2c for http)                  │   wrap stream as net.Conn
      │                                                  │
      │  RegisterRequest frame ───────────────────────────▶  claim a session
      │   { session_id?, client_version, features:[grpc] }   • mint secret + publicID
      │                                                  │   • negotiate features
      │  ◀───────────────────────────────────────  RegisterReply frame
      │     { session_id, public_url, features:[grpc] }  │
      │                                                  │
      │  ===== both wrap the SAME stream in yamux ====    │
      │   fleetd = yamux CLIENT      gateway = yamux SERVER
      │                                                  │
      │  ◀──────── gateway opens streams ────────────────│   (one per inbound request)
      │  ───────── fleetd accepts & serves ──────────────▶
```

(Because HTTP/2 only lets the *client* open streams, fleetd opens this one bidi
stream and yamux multiplexes the gateway's inbound requests back down it — yamux
provides the reverse‑direction multiplexing HTTP/2 won't. The register handshake is
JSON frames at the head of the stream; yamux owns the rest.)

Key points:

- **Sticky sessions.** The gateway mints a **256‑bit secret** (the reclaim
  credential) *and* a separate **256‑bit public id**. The public URL is
  `https://<gateway>/mcp/<publicID>` (and `/grpc/<publicID>`). The secret is
  returned to `fleetd`, persisted in `~/.fleet/gateway_session.json`, and resent
  on reconnect — so a daemon that drops and reconnects recovers the **same URL**.
  The secret is **never** placed in the URL, so a holder of the URL cannot hijack
  the tunnel. A disconnected session is held for a grace TTL (~10 min) before the
  reaper frees its URL.

- **Feature negotiation.** `fleetd` advertises the capabilities it supports
  (`features: ["grpc"]`); the gateway replies with the **intersection** of what
  *both* support. gRPC tunneling (and the per‑stream tag in §4) only activates
  when `grpc` is in that negotiated set. An un‑upgraded gateway or daemon simply
  negotiates nothing and the tunnel stays the legacy MCP‑only wire — so a
  version‑skewed rollout is always safe.

---

## 4. One tunnel, two protocols: the stream tag

The gateway opens one yamux stream per inbound request. Once `grpc` is
negotiated, every stream the gateway opens begins with a single **tag byte** so
`fleetd` knows how to handle it:

| Tag | Value | Meaning |
|-----|-------|---------|
| `TagMCP` | `0x00` | The stream carries an **HTTP request** for the loopback MCP server. |
| `TagGRPC` | `0x01` | The stream carries a **native‑gRPC (HTTP/2, h2c)** connection the gateway reverse‑proxies to the tunnel gRPC server. |

The tag is written by the gateway as the *first bytes* of the stream, before any
payload, so `fleetd` reads it immediately without guessing. On a legacy
(un‑negotiated) session, **no tag is written** and every stream is plain MCP HTTP —
identical to the original wire.

On the daemon side, `serveTunnel` is the demultiplexer:

```
 fleetd: serveTunnel(session)
      │
      ├─ Accept() a yamux stream
      │      │
      │      ├─ ReadTag()
      │      │     ├─ 0x00 → push to the MCP http.Server   ──▶ reverse‑proxy to 127.0.0.1:<mcpPort>
      │      │     ├─ 0x01 → push to the gRPC ChanListener  ──▶ the token‑gated tunnel grpc.Server
      │      │     └─ else → close the stream
      │      ▼
      └─ (loop)
```

(The MCP and gRPC servers are each fed by an in‑memory `ChanListener` — a
`net.Listener` whose connections are *pushed* in by the demux, so neither needs a
real port.)

---

## 5. The MCP request path

A remote MCP agent is configured with the public URL
`https://<gateway>/mcp/<publicID>` and the **bearer token**.

```
 MCP agent                gateway (proxy.go)            fleetd                     loopback MCP
    │                          │                          │                       (127.0.0.1:<port>,
    │  POST /mcp/<id>          │                          │                        bearer‑gated)
    │  Authorization: Bearer ──▶ lookup session by <id>   │                            │
    │  (Accept: …event-stream) │ httputil.ReverseProxy:   │                            │
    │                          │  • DialContext = session.Open()                       │
    │                          │       └─ writes TagMCP (0x00) ──────────yamux stream──▶ serveTunnel
    │                          │  • strip /mcp/<id> → "/"  │   reads tag → MCP http.Server
    │                          │  • forward Authorization  │   → httputil.ReverseProxy │
    │                          │  • FlushInterval=-1 (SSE) │   • Host = 127.0.0.1:<port>│  (DNS‑rebinding)
    │                          │                          │   • Authorization passthrough ▶ mcpAuth ─▶ MCP SDK
    │  ◀───────────── streamed response (incl. SSE) ───────────────────────────────────────────────
```

What each hop does:

1. **Gateway** matches `/mcp/{id}`, looks up the session, and reverse‑proxies the
   request over a fresh yamux stream (tagged `TagMCP`). It strips the
   `/mcp/<id>` prefix, forwards the `Authorization` header untouched, and sets
   `FlushInterval=-1` so **SSE streams flush immediately**.
2. **fleetd** reads the tag, hands the stream to its MCP `http.Server`, which
   reverse‑proxies to `127.0.0.1:<mcpPort>`. It sets the request `Host` to the
   loopback target (the MCP SDK's **DNS‑rebinding protection** 403s a loopback
   server whose `Host` isn't loopback) and leaves `Authorization` untouched.
3. **The loopback MCP server** validates the bearer token (`mcpAuth`) and runs
   the MCP Streamable‑HTTP handler. SSE responses stream back the same way.

The token is the boundary: the gateway never checks it; the **loopback MCP
server** does.

---

## 6. The gRPC connection path

gRPC is HTTP/2, and its request *path* is the method name — so it can't be
path‑routed like MCP. The gateway therefore serves gRPC on a **dedicated listener**
(`--grpc-addr`, default `:50051`) that speaks native HTTP/2 (h2c when cert‑less,
h2 via ALPN under TLS), and routes by a `fleet-session` **metadata header** instead
of the path. That makes it a clean L7 gRPC endpoint a standard gRPC reverse proxy
(e.g. Traefik) can front. The gateway is itself a **grpc‑go transparent proxy** here
(`grpc.UnknownServiceHandler` + a raw passthrough codec — the well‑known grpc‑proxy
pattern), forwarding each stream down a `TagGRPC` tunnel stream to the daemon's own
`grpc.Server`. A grpc‑level proxy (not an `httputil.ReverseProxy`) is required so
grpc‑go manages HTTP/2 framing and **trailers** (`grpc-status`) on both hops —
including a *trailers‑only* error response, which an HTTP reverse proxy drops.

```
 remote `fleet`             gateway (grpc.go, :50051)        fleetd                tunnel gRPC server
    │                            │                            │                  (auth interceptor,
    │  native gRPC dial          │                            │                   FleetService)
    │  RPC + metadata:           │                            │                       │
    │   fleet-session: <id> ─────▶ route by header            │                       │
    │   authorization: Bearer    │  (InvalidArgument no hdr / │                       │
    │                            │   NotFound no session)     │                       │
    │                            │  grpc proxy (raw codec) ──TagGRPC(0x01)──yamux──▶ serveTunnel
    │                            │   per-session grpc.ClientConn  reads tag → ChanListener ─▶ grpc.Server
    │  ◀──────────────────────── responses / streams / trailers (grpc-status) ─────────
```

What each hop does:

1. **Remote client** (`internal/fleetclient`): `FLEET_GATEWAY=https://gw:50051/<id>`
   selects a `gatewayEndpoint`. It dials the gateway as an **ordinary gRPC target**
   — verified TLS for `https` (system roots), or plaintext h2c for `http`. Every RPC
   carries two metadata headers: `fleet-session: <id>` (routing) and
   `authorization: Bearer <token>` (auth).
2. **Gateway** reads `fleet-session` (`InvalidArgument` if absent; `NotFound` unless
   the session exists *and* negotiated gRPC) and proxies the stream to a per‑session
   `grpc.ClientConn` whose dialer opens a `TagGRPC` tunnel stream (h2c). It forwards
   metadata, messages, and trailers verbatim, and re‑dials a fresh stream on the live
   tunnel after a reconnect.
3. **fleetd** reads the tag, pushes the stream to the **tunnel gRPC server** (via
   the `ChanListener`). That server runs the *same* `FleetService` as the local
   unix socket but behind a **bearer‑token metadata interceptor** — the gateway only
   routes; `fleet-session` is not a credential.

All of gRPC works through the reverse proxy — unary, server‑streaming (`Watch`,
`Logs`, jobs), and **bidirectional streaming** (`Exec`). The proxy flushes per write
(`FlushInterval: -1`) so streaming isn't buffered, and forwards HTTP/2 trailers
(`grpc-status`) so per‑RPC status propagates.

> **Note:** the gRPC API rides the *same* tunnel and the *same* on/off toggle as
> MCP, and reuses the *same* bearer token. Enabling remote MCP exposes both.

---

## 7. Authentication and trust model

| | MCP path | gRPC path |
|---|---|---|
| **What gates it** | `mcpAuth` middleware on the loopback MCP server | a bearer‑token **metadata interceptor** on the tunnel gRPC server |
| **Where it's checked** | at `fleetd` (the gateway holds no token) | at `fleetd` (same) |
| **Credential** | `Authorization: Bearer <token>` | `authorization: Bearer <token>` metadata |

The single secret is `~/.fleet/mcp.token` — **one token gates both paths**.

Security properties:

- **No gateway authentication.** The gateway authorizes nobody; it routes bytes.
  Isolation comes from the **unguessable 256‑bit public id** in the URL.
- **The token is the real boundary.** The gateway forwards the credential
  untouched; the daemon validates it. A holder of the public URL can open tunnel
  streams, but **every request without a valid token is rejected** by `fleetd`.
- **No hijack.** The reconnect *secret* is distinct from the public id and never
  appears in the URL, so a URL holder cannot re‑register the session.
- **The local unix socket stays auth‑less** (same‑user, `0600`) — only the
  *tunnel‑facing* servers require the token.
- **Public traffic is TLS — terminated either by the gateway or by a proxy in
  front of it.** With `--tls-cert`/`--tls-key`, the gateway terminates TLS itself:
  `fleetd` verifies its certificate on dial‑in (so use a publicly‑trusted cert) and
  agents/clients connect over HTTPS. Without a cert, the gateway serves **plain
  HTTP/TCP** and is meant to sit behind a TLS‑terminating reverse proxy (see
  **Deployment modes** below). Either way the gateway sees plaintext traffic and the
  token — run a gateway (and proxy) you trust. Reusing the MCP token for the gRPC
  path means that one secret now grants **full daemon control over the internet**,
  so treat it accordingly.
- **DoS bounds:** a `MaxSessions` cap (registration is rejected past it), a
  per‑registration handshake timeout (a stream that never sends `RegisterRequest`
  is dropped), a **pending‑handshake semaphore** sized `MaxSessions +
  maxPendingHandshakes` that *sheds* (gRPC `ResourceExhausted`) excess concurrent
  Register handlers before they can pile up, and a per‑connection
  `MaxConcurrentStreams` cap. (HTTP/2 flow control bounds bytes‑per‑stream, not the
  stream *count* — the semaphore is what bounds the count.)

---

## 7a. Deployment modes: direct TLS vs. reverse proxy

TLS on the listeners is **optional and all‑or‑nothing** (`gateway.New` rejects a
lone cert or key):

- **Direct TLS** (`--tls-cert` + `--tls-key`): the public listener is `tls.Listen`
  and the gRPC server gets `grpc.Creds` (ALPN h2). Standalone mode — point a public
  DNS name at the host, give it a publicly‑trusted cert, done.
- **Reverse‑proxy / TLS‑terminated upstream** (no cert): the public listener is
  plain `net.Listen` (HTTP/1.1) and the gRPC server serves **h2c**; a proxy in front
  terminates TLS. Set `--public-url` to the externally‑visible `https://…` (or
  `http://…`) the proxy serves; the registry reflects that scheme back verbatim.

The clients are symmetric: both the daemon **registration** dialer
(`internal/server/remote`, which opens the gRPC `Register` stream) and the remote
`fleet` client (`internal/fleetclient`) accept `https://` (verified TLS against the
OS system roots) **or** `http://` (plaintext h2c), defaulting the port to 443/80.

> ✅ **Both listeners are L7 — no raw TCP.** Public MCP (HTTP/1.1) fronts behind a
> Traefik **HTTP** route; the gRPC listener (HTTP/2) — which carries remote control
> **and** fleetd registration — fronts behind a Traefik **gRPC** route (h2c
> backend). There is no L4/`IngressRouteTCP` requirement anymore. See the README's
> *Behind a TLS‑terminating reverse proxy* section for concrete manifests.

---

## 8. Lifecycle & resilience

- **Reconnect.** If the tunnel drops, `remote.Manager` reconnects with jittered
  exponential backoff, resending its secret so the gateway re‑binds the same URL.
- **Reaping.** The gateway holds a disconnected session (URL reserved) for a grace
  TTL, then a reaper frees it — so a `kill -9`'d daemon's URL is released, but a
  brief network blip keeps the URL stable.
- **Status.** `fleetd` publishes the live connection state and the assigned public
  URL over its Watch stream, so the TUI's settings page shows it in real time. The
  computed Public MCP URL is *never* stored in config — only pushed over Watch.

---

## 9. Putting it together

```
                                 ┌──────────────────────── fleet gateway ───────────────────────┐
   remote MCP agent  ──HTTP/1──▶ │  public listener (:443)  /mcp/<id> → ReverseProxy ─┐          │
   (Claude, etc.)                │                                                    │          │
   remote `fleet`    ──gRPC────▶ │  gRPC listener (:50051)                            │ registry │
   (FLEET_GATEWAY)               │   • control RPC  h2 + fleet-session → proxy ───────┤ (id→tun) │
                                 │   • Register     ◀──fleetd opens bidi stream───────┼── fleetd
                                 └────────────────────────────────────────────────────┼──────────┘
                                                                 │  one gRPC bidi stream,
                                                                 │  yamux, tagged streams
                                              ┌──────────────────▼───────────────────┐
                                              │                fleetd                 │
                                              │  remote.Manager → serveTunnel (demux) │
                                              │     ├─ TagMCP  → 127.0.0.1:<mcpPort>   │  (bearer‑gated MCP)
                                              │     └─ TagGRPC → tunnel gRPC server    │  (bearer‑gated FleetService)
                                              │  local unix socket (auth‑less, 0600)  │  ← local CLI/TUI
                                              └───────────────────────────────────────┘
```

---

## 10. Status of the gRPC path

Everything that the daemon's gRPC API exposes today works remotely: `GetState`,
`Watch`, the lifecycle jobs (`up`/`down`/`start`/`stop`/`clone`), `Logs`, config,
and mutations. **Interactive `exec` (a remote shell) is not yet supported** over
the gateway — `fleet exec` currently runs the resolved command on the *client's*
host, so a true remote shell needs a server‑side bidirectional `Exec` handler.
The bidi *transport* is proven (the end‑to‑end test drives an interleaved
bidirectional stream through the gateway); only the server‑side `Exec` handler
remains.
