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

The request fails with `{"login_code":"","status":"ERROR"}` and auth-hub logs
it. Dragonite reads that as its retryable `ErrAuthNoToken` and tries again;
with N upstreams roughly 1/N of attempts fail until you pull the dead one from
the config.

auth-hub deliberately never emits `INVALID` or `BANNED` of its own accord.
Dragonite responds to those by permanently calling `MarkInvalid()` /
`MarkAuthBanned()` on the account, so a mere connection error must not be able
to trigger them. Real `INVALID`/`BANNED` verdicts from an upstream pass
straight through.

## Not included

- **Health checks / failover to the next upstream on error.** A dead upstream
  eats its share of requests until removed.
- **Weighting.** Every upstream gets an equal share.
- **Refresh traffic.** Dragonite's token refresh talks to PTC directly and
  never touches `remote_auth_url`, so it doesn't pass through here.
