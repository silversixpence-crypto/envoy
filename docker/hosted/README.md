# Hosted node image

`Dockerfile.hosted` builds an Envoy node for a host with no durable disk. The database
lives at `/data/trisa.db` on the container's own filesystem; [Litestream][ls] streams
its WAL to an S3-compatible bucket about once a second and restores it before Envoy
starts. Kill the container, start a new one with the same environment, and it comes
back with its transfers, its counterparties and its API keys.

`/data` is a plain directory, not a `VOLUME`. On Cloudflare Containers it is ephemeral
and the bucket is the only durable copy. On a VPS you mount a volume over it and the
bucket becomes the backup instead (see [On a VPS](#on-a-vps)).

The image is TRP-only in practice, not by construction: the TRISA gRPC node is switched
off through the environment, the same way the `envoy-trp-only` demo does it. Nothing in
the image forbids turning it back on.

## Build

```sh
docker build -f Dockerfile.hosted \
  --build-arg VERSION=hosted-$(git rev-parse --short=7 HEAD) \
  -t trisa/envoy:hosted .
```

`VERSION` goes into `pkg.GitVersion`, so `envoy --version` and `GET /v1/status` report
which commit the node is running.

## Boot sequence

The entrypoint is `docker/hosted/entrypoint.sh`. Four steps, each logged with a
`[hosted-entrypoint]` prefix:

1. Decode the secrets from the environment into `/run/node` (directory 0700, files
   0600) and export the `TRISA_*` variables that point at them.
2. `litestream restore -if-db-not-exists -if-replica-exists /data/trisa.db`.
3. Check that a database now exists. If not, demand `NODE_BOOTSTRAP=1`.
4. `exec litestream replicate -exec "/usr/local/bin/envoy serve"`. Litestream is PID 1
   and exits when Envoy exits.

Step 3 logs which of three paths the node took: `restored`, `existing-volume` or
`bootstrap`. That line is the one to grep for when a node comes up empty.

## Running envoy subcommands in a live node

`docker exec` gets the container's configured environment, not the variables the
entrypoint exported, so `envoy apikey:create` inside a running node fails with
`specify certificates path`. Step 1 writes those exports to `/run/node/env`
(mode 0600). Source it:

```sh
docker exec trponly-hosted-alpha \
  sh -c '. /run/node/env && exec /usr/local/bin/envoy apikey:create all'
```

## Fail-closed rules

An empty database that replicates itself over a real history destroys the node. Every
check below exists to prevent that.

- No secrets in the environment and no `TRISA_NODE_CERTS` set: exit 1. The node has no
  identity and no sealing key, so any database it built would be unreadable anyway.
- `NODE_JWT_KEY_ID` is not a ULID, or `NODE_JWT_KEY_B64` does not decode to a PEM
  private key: exit 1. Envoy would otherwise generate a throwaway signing key at boot
  and invalidate every token it ever issued.
- `litestream restore` exits non-zero: exit 1. Wrong credentials, wrong endpoint and a
  corrupt generation all land here. `-if-replica-exists` already returns 0 for the one
  case that is legitimately empty, a prefix with no backups, so a non-zero exit always
  means the replica is there and unreadable.
- The restore produced nothing and `NODE_BOOTSTRAP` is not `1`: exit 1 with `no replica
  for this node and NODE_BOOTSTRAP is not set`. This is the guard against a typo in
  `LITESTREAM_PATH` quietly starting a brand new node under someone else's slug.
- `LITESTREAM_BUCKET`, `LITESTREAM_PATH` or `LITESTREAM_ENDPOINT` missing: exit 1.

`NODE_BOOTSTRAP=1` is for the first boot of a new node only. Leaving it set turns every
one of these failures back into silent data loss.

## Environment

### Secrets

| Variable | Required | What it is |
| --- | --- | --- |
| `NODE_CERTS_B64` | yes | base64 of `node.pem.gz`: gzip of the node certificate, the CA certificate and the node private key, concatenated as PEM. Written to `/run/node/node.pem.gz` and exported as `TRISA_NODE_CERTS` and `TRISA_TRP_CERTS`. |
| `NODE_POOL_B64` | yes | base64 of `pool.pem.gz`: gzip of the CA certificate. Written to `/run/node/pool.pem.gz` and exported as `TRISA_NODE_POOL` and `TRISA_TRP_POOL`. |
| `NODE_JWT_KEY_B64` | yes | base64 of an RSA private key in PEM (PKCS#1 or PKCS#8). Written to `/run/node/jwt.pem`. |
| `NODE_JWT_KEY_ID` | with the key | ULID naming that key. Becomes the `kid` in every JWT the node issues, via `TRISA_WEB_AUTH_KEYS=<id>:/run/node/jwt.pem`. |

Set `TRISA_NODE_CERTS`, `TRISA_NODE_POOL`, `TRISA_TRP_CERTS`, `TRISA_TRP_POOL` or
`TRISA_WEB_AUTH_KEYS` yourself and the entrypoint leaves them alone. That is how a VPS
bind-mounts the files instead of passing them through the environment.

### Replication

| Variable | Default | What it is |
| --- | --- | --- |
| `LITESTREAM_BUCKET` | none, required | Bucket holding every node's data. |
| `LITESTREAM_PATH` | none, required | Prefix inside the bucket. One node, one prefix, forever. |
| `LITESTREAM_ENDPOINT` | none, required | S3 endpoint URL. `https://<account>.r2.cloudflarestorage.com` for R2, `http://rustfs:9000` on the local bench. Plain HTTP is accepted. |
| `LITESTREAM_REGION` | `us-east-1` | Region. R2 ignores it; the SDK still wants one. |
| `LITESTREAM_ACCESS_KEY_ID` | none, required | Read by Litestream itself, never named in `litestream.yml`. |
| `LITESTREAM_SECRET_ACCESS_KEY` | none, required | As above. |
| `NODE_BOOTSTRAP` | `0` | `1` allows the node to start with an empty prefix. First boot only. |

The entrypoint applies the `LITESTREAM_REGION` default before exec'ing Litestream
because Litestream expands `${VAR}` in its config with Go's `os.Expand` and has no
`${VAR:-default}` syntax. Written in the config file, `${LITESTREAM_REGION:-us-east-1}`
is read as a variable whose name contains `:-us-east-1` and expands to nothing.

### Envoy

Everything else is ordinary Envoy configuration and the image sets none of it. A
TRP-only node needs at least `TRISA_DATABASE_URL=sqlite3:////data/trisa.db`,
`TRISA_NODE_ENABLED=false`, `TRISA_DIRECTORY_SYNC_ENABLED=false`, `TRISA_TRP_ENABLED=true`,
`TRISA_TRP_CALLBACK_KEY` and the `TRISA_WEB_*` origin settings.
`envoy-trp-only/docker-compose.hosted.yaml` in the demo repo is a working example.

## Retry the TRP callbacks

Litestream checkpoints the WAL while Envoy is writing, and Envoy's write transactions
start deferred, so a write that has to upgrade the lock during a checkpoint gets
SQLite's `database is locked` immediately rather than waiting out the busy timeout. In a
300-transfer loop on this bench that hit 6 of 300 transfers, always on the sender's
`POST /transfers/:id/resolve/:token`, which answered 500 in about 12 ms.

The transfer stays `pending` and the callback succeeds on the next attempt, so nothing is
lost, but anything driving these nodes has to retry a 500 on the TRP endpoints. A fork
change (`_txlock=immediate` on the SQLite connection) would remove the class entirely.

## One writer per prefix

Litestream assumes it owns the database file it replicates. Two containers writing to
one prefix will both push generations into it, and a later restore gets whichever
generation the manifest happens to point at. The other node's transfers are gone. This
is not something the image can enforce, so whatever creates nodes has to: one slug, one
prefix, one running container.

## On a VPS

Mount a volume over `/data` and the entrypoint's behaviour changes on its own.
`-if-db-not-exists` sees the file, returns 0 without downloading anything, and step 3
logs `existing-volume`. Litestream still replicates to the bucket, so the bucket is a
backup rather than the primary copy, and a rebuilt VPS restores from it the same way a
container does.

```sh
docker run -d --name envoy-acme \
  -v /srv/envoy/acme:/data \
  --env-file /srv/envoy/acme.env \
  -p 8000:8000 -p 8200:8200 \
  trisa/envoy:hosted
```

Restoring onto a fresh volume is the same command with an empty `/srv/envoy/acme` and
`NODE_BOOTSTRAP` unset, which is exactly the fail-closed path: if the bucket has
nothing, the container refuses to start rather than inventing a new node.

[ls]: https://litestream.io/reference/config/
