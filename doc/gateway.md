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
so many independent requests/streams ride over it concurrently. The gateway is
otherwise a *dumb pipe* — it routes bytes and never inspects the payload, which
is what lets it carry both HTTP (MCP) and native HTTP/2 (gRPC).

```
        Behind NAT / firewall                          Public internet
 ┌───────────────────────────────┐            ┌──────────────────────────┐
 │            fleetd             │            │       fleet gateway       │
 │                               │  dials out │                          │
 │  remote.Manager  ───────────────────────────▶  control listener       │
 │  (one TLS conn, yamux)        │  (TLS)     │  (:8443)                  │
 │                               │◀───────────────  pushes streams down   │
 │  loopback MCP  (127.0.0.1)    │            │  public listener (:443)  │◀── agents / remote `fleet`
 │  loopback gRPC (in‑memory)    │            │                          │
 └───────────────────────────────┘            └──────────────────────────┘
```

---

## 2. Component map

| Component | Package | Role |
|-----------|---------|------|
| **Shared tunnel protocol** | `internal/tunnel` | The register handshake (length‑prefixed JSON), the yamux session helpers, and the per‑stream **tag** byte. Imported by *both* ends. |
| **fleetd tunnel client** | `internal/server/remote` | `Manager` dials the gateway, registers, and serves inbound streams; `serveTunnel` demuxes them to MCP or gRPC. |
| **fleetd MCP server** | `internal/server/mcp.go` | The loopback MCP server (`127.0.0.1:<port>`), bearer‑token gated. |
| **fleetd gRPC server (tunnel)** | `internal/server` | A second `grpc.Server` (same `FleetService`) behind a bearer‑token interceptor, fed by the demux. The local unix‑socket server stays auth‑less. |
| **The gateway** | `internal/gateway` | Two TLS listeners (control + public), the session registry, and the `/mcp` + `/grpc` routes. An **isolated module** — imports only `internal/tunnel`. |
| **Remote `fleet` client** | `internal/fleetclient` | `gatewayEndpoint` + a CONNECT dialer that reaches the daemon's gRPC API through the gateway. |

---

## 3. Registration (the control connection)

When the user enables remote MCP, `fleetd`'s `remote.Manager` dials the gateway's
**control** listener and performs a tiny handshake on the raw TLS connection
*before* yamux takes over:

```
 fleetd (remote.Manager)                         gateway (control.go)
      │                                                  │
      │  TLS dial ───────────────────────────────────────▶  accept
      │                                                  │
      │  RegisterRequest ─────────────────────────────────▶  claim a session
      │   { session_id?, client_version, features:[grpc] }   • mint secret + publicID
      │                                                  │   • negotiate features
      │  ◀───────────────────────────────────────  RegisterReply
      │     { session_id, public_url, features:[grpc] }  │
      │                                                  │
      │  ===== both wrap the SAME conn in yamux =====    │
      │   fleetd = yamux CLIENT      gateway = yamux SERVER
      │                                                  │
      │  ◀──────── gateway opens streams ────────────────│   (one per inbound request)
      │  ───────── fleetd accepts & serves ──────────────▶
```

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
| `TagGRPC` | `0x01` | The stream carries a **raw native‑gRPC (HTTP/2)** connection to splice to the gRPC server. |

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
path‑routed like MCP. Instead the gateway tunnels the **whole gRPC connection**
as raw bytes. The remote `fleet` client first does a small CONNECT‑style
handshake to pick the session, then runs native gRPC over the resulting pipe.

```
 remote `fleet`             gateway (proxy.go /grpc)        fleetd                tunnel gRPC server
    │                            │                            │                  (auth interceptor,
    │  TLS dial gateway          │                            │                   FleetService)
    │  GET /grpc/<id> ───────────▶ lookup session (404 if no  │                       │
    │                            │  session / grpc not negot.)│                       │
    │                            │  • session.Open() ──TagGRPC(0x01)──yamux stream──▶ serveTunnel
    │                            │  • HIJACK the conn         │   reads tag → ChanListener ─▶ grpc.Server
    │  ◀── "HTTP/1.1 200         │                            │                       │
    │       Connection           │   ╔════════ raw splice ════╗                       │
    │       Established"         │   ║ two io.Copy goroutines ║                       │
    │                            │   ║ (byte‑transparent)     ║                       │
    │  ── client h2 preface ─────────────────────────────────────────────────────────▶ (native h2)
    │  ── RPCs (unary / server‑stream / BIDI) — metadata: Authorization: Bearer ─────▶ interceptor → FleetService
    │  ◀──────────────────────── responses / streams ────────────────────────────────
```

What each hop does:

1. **Remote client** (`internal/fleetclient`): `FLEET_GATEWAY=https://gw/grpc/<id>`
   selects a `gatewayEndpoint`. Its gRPC dialer TLS‑dials the gateway, sends
   `GET /grpc/<id>`, and waits for `200 Connection Established`. It then hands the
   raw conn to gRPC, which speaks **prior‑knowledge h2c** over it. Every RPC
   carries `authorization: Bearer <token>` as metadata.
2. **Gateway** matches `/grpc/{id}` (404 unless the session exists *and*
   negotiated gRPC), opens a `TagGRPC` stream, **hijacks** the public connection,
   replies `200`, and then runs a **raw byte splice** (two `io.Copy` goroutines)
   between the agent's connection and the yamux stream. It never parses the h2
   bytes.
3. **fleetd** reads the tag, pushes the stream to the **tunnel gRPC server** (via
   the `ChanListener`). That server runs the *same* `FleetService` as the local
   unix socket but behind a **bearer‑token metadata interceptor**.

Because the splice is byte‑transparent, **all** of gRPC works unchanged — unary,
server‑streaming (`Watch`, `Logs`, jobs), and **bidirectional streaming**
(`Exec`). gRPC's half‑close, flow control, and trailers are in‑band HTTP/2 frames
that ride through the splice untouched; teardown happens only when the whole
connection ends, never per‑RPC.

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
- **All tunnel traffic is TLS.** `fleetd` verifies the gateway's certificate when
  it dials in (so use a publicly‑trusted cert), and agents/clients connect over
  HTTPS. The gateway **terminates** TLS, however, so its operator can see the
  plaintext traffic and the token — run a gateway you trust. Reusing the MCP token
  for the gRPC path means that one secret now grants **full daemon control over
  the internet**, so treat it accordingly.
- **DoS bounds:** a `MaxSessions` cap, a per‑connection handshake timeout, a
  load‑shedding control‑accept loop, and yamux flow control bound resource use on
  the unauthenticated public surface.

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
   remote MCP agent  ──HTTPS──▶  │  public listener (:443)                                       │
   (Claude, etc.)                │    /mcp/<id>   → ReverseProxy ─┐                               │
                                 │    /grpc/<id>  → hijack+splice ┤                               │
   remote `fleet`    ──HTTPS──▶  │                               │  session registry (id→tunnel) │
   (FLEET_GATEWAY)               │  control listener (:8443) ◀───┼── fleetd dials in (TLS)        │
                                 └───────────────────────────────┼───────────────────────────────┘
                                                                 │  one TLS conn, yamux,
                                                                 │  tagged streams
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
