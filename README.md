# auth-hub

A round-robin reverse proxy for Dragonite's remote auth.

Dragonite takes exactly one `remote_auth_url` per account provider. auth-hub sits
in that slot and spreads the requests across as many real auth servers as you like, each with its own secret.

```
                       ┌─> auth-1  (secret-1)
Dragonite ──> auth-hub ┼─> auth-2  (secret-2)
                       └─> auth-3  (secret-3)
```

It's a straight proxy: the request body and the response body pass through
untouched. The only things it rewrites are the target URL and the
`X-Remote-Auth-Secret` header.

## Build

```sh
go build
```

## Configure

Copy `config.toml.example` to `config.toml` and fill it in. Then point Dragonite
at it:

```toml
[auth.ptc]
enable = true
remote_auth_url = "http://auth-hub:9090/ptc"
remote_auth_secret = "the same value as auth-hub's `secret`"
```

One `[[pool]]` per account provider type, each on its own path.

Upstreams within a pool are interchangeable — that's what makes rotating between
them safe. Pools exist because that stops at the provider boundary: the login
URL Dragonite sends is provider specific, so a PTC auth server can't service a
Google login. Only `ptc` and `g` use remote auth; `nk` authenticates on its own
and never reaches auth-hub.

Dragonite calls the Google provider `g`, so its section is `[auth.g]` —
`[auth.google]` is rejected as an unknown provider. The path is auth-hub's own,
so name it whatever you like as long as Dragonite points at it.

## Run

```sh
./auth-hub -config config.toml
```

### Docker

```sh
mkdir -p config && cp config.toml.example config/config.toml
$EDITOR config/config.toml
docker compose up -d
```

The compose file mounts the `config/` directory rather than the file itself, so
that editing the config keeps working regardless of how your editor saves.

## Reloading

Edit `config.toml` and auth-hub picks it up within about five seconds. Or send
`SIGHUP` to apply it immediately, the same way Dragonite reloads:

```sh
kill -HUP $(pidof auth-hub)     # or: docker compose kill -s HUP auth-hub
```

Upstreams, secrets and whole pools can all be changed this way. `listen` is the
exception — the port is already bound, so changing it needs a restart, and a
reload that tries will say so.

A config that doesn't parse or doesn't validate is logged and **ignored**:
auth-hub carries on with the last good one rather than dropping auth on a typo.

## How secrets work

There are two layers, and they're deliberately different values:

- **`secret`** — what Dragonite sends. auth-hub checks it on every request
  (constant-time) and returns `403` if it's wrong. Without it auth-hub would be
  an open relay onto your auth servers, so it's required.
- **`pool.upstream.secret`** — what each real auth server expects. auth-hub
  swaps Dragonite's secret out for this one before forwarding. Dragonite's
  secret is never sent upstream.

This is why you can pool servers that don't share a secret.

## Behaviour when an upstream is down

If a request fails to reach its upstream — refused, reset, timed out — auth-hub
moves on to the next one and tries again, up to once per upstream. A dead
upstream is invisible to Dragonite as long as one live upstream is left.

Two things deliberately don't trigger a retry:

- **A reply that arrived.** Only transport failures fail over. If an upstream
  answers, that answer is the answer, including `INVALID` and `BANNED`.
- **A caller that gave up.** If Dragonite has hung up or spent its
  `remote_auth_timeout_seconds`, nothing is listening, and trying more
  upstreams would just burn logins.

When every upstream has been tried, the request returns
`{"login_code":"","status":"ERROR"}` and auth-hub logs each failure. Dragonite
reads that as its retryable `ErrAuthNoToken`.

auth-hub never emits `INVALID` or `BANNED` of its own accord. Dragonite
responds to those by permanently calling `MarkInvalid()` / `MarkAuthBanned()`
on the account, so a mere connection error must not be able to trigger them.

## Weights

Give an upstream a `weight` to change its share of a pool's traffic:

```toml
  [[pool.upstream]]
  url = "http://big-box:5090/api/v1/login-code"
  weight = 3        # takes 3 logins for every 1 the next one takes

  [[pool.upstream]]
  url = "http://little-box:5090/api/v1/login-code"
  weight = 1
```

`weight` defaults to 1, so leaving it out everywhere splits traffic evenly —
exactly what an unweighted config has always done.

Turns are spread rather than clumped: weights 3 and 1 rotate `a a b a`, not
`a a a b`, so concurrent logins don't all pile onto the heavy upstream at once.

`weight = 0` **drains** an upstream — it keeps its config but takes no traffic,
not even as a failover target. Useful before restarting one. At least one
upstream in a pool must be above 0. Weights are capped at 1000.

Weights only decide where a request *starts*. Failover ignores them and walks
the rest of the pool, because at that point it's about finding anything alive.

## Not included

- **Health checks.** A dead upstream is discovered per request, by failing over
  rather than by being probed in the background. It still costs one failed dial
  per turn through the rotation.
- **Refresh traffic.** Dragonite's token refresh talks to PTC directly and
  never touches `remote_auth_url`, so it doesn't pass through here.
