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

One `[[pool]]` per account provider type, each on its own path. Keep PTC and
Google in separate pools — a PTC auth server can't service a Google login URL.

## Run

```sh
./auth-hub -config config.toml
```

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

## Not included

- **Health checks.** A dead upstream is discovered per request, by failing over
  rather than by being probed in the background. It still costs one failed dial
  per turn through the rotation.
- **Weighting.** Every upstream gets an equal share.
- **Refresh traffic.** Dragonite's token refresh talks to PTC directly and
  never touches `remote_auth_url`, so it doesn't pass through here.
