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
2. Pick the storage credential mode (see [Storage credentials](#storage-credentials)),
   fetch credentials once in process mode, then
   `litestream restore -if-db-not-exists -if-replica-exists /data/trisa.db`.
3. Check that a database now exists. If not, demand `NODE_BOOTSTRAP=1`.
   3b. On the bootstrap path only, and only when `NODE_BOOTSTRAP_CALLBACK_URL` is set:
   mint the node's first API key and POST it to that URL. See
   [The bootstrap callback](#the-bootstrap-callback).
4. `exec litestream replicate -exec "/usr/local/bin/envoy serve"`. Litestream is PID 1
   and exits when Envoy exits.

Step 3 logs which of three paths the node took: `restored`, `existing-volume` or
`bootstrap`. That line is the one to grep for when a node comes up empty.

## Storage credentials

Litestream reaches the bucket through the AWS SDK for Go v1, and the node gets its
credentials for that one of two ways. The entrypoint picks the mode explicitly in step 2
and logs `mode=process` or `mode=static`.

### Process mode (Cloudflare)

Chosen when `NODE_STORAGE_CREDENTIALS_URL` is set. The Worker issues each node R2
credentials scoped to its own prefix, valid for 24 hours and rotated at 18. Environment
variables are read once, so they could only be rotated by restarting the container.
Instead:

- `Dockerfile.hosted` installs `/etc/aws/config` with
  `credential_process = /usr/local/bin/storage-credentials` under `[default]`, and sets
  `AWS_CONFIG_FILE=/etc/aws/config` and `AWS_SDK_LOAD_CONFIG=1`.
- The SDK runs the helper when Litestream first needs credentials and again whenever
  the `Expiration` it returned has passed. New keys are picked up with no restart and no
  failed requests.

The contract with the Worker:

- The container gets `NODE_STORAGE_CREDENTIALS_URL` (`<public_base>/_storage-credentials`)
  and `NODE_STORAGE_CREDENTIALS_TOKEN` (64 hex characters).
- `GET $NODE_STORAGE_CREDENTIALS_URL` with `Authorization: Bearer
  $NODE_STORAGE_CREDENTIALS_TOKEN` answers 200 with exactly the credential_process
  document: `{"Version":1,"AccessKeyId":…,"SecretAccessKey":…,"SessionToken":…,"Expiration":…}`.
  401, 503 or 410 otherwise.

The helper (`docker/hosted/storage-credentials.sh`) reads the token from the
environment each time it runs and hands it to curl through a 0600 config file in a
private temporary directory that is removed on exit, so the token never appears in a
process argument list. It allows 5 seconds to connect and 15 in total, treats anything
but a 200 as failure (redirects are not followed), checks that the body has
`"Version":1`, an `AccessKeyId` and a `SecretAccessKey`, and then prints the body
unchanged. On failure it exits 1 with one line on stderr naming the URL and the HTTP
status or connection error, never the token or the body.

At boot the entrypoint:

1. Requires `NODE_STORAGE_CREDENTIALS_TOKEN`; missing, exit 1.
2. Refuses to start if any of `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
   `AWS_SESSION_TOKEN` (or the SDK's aliases `AWS_ACCESS_KEY`, `AWS_SECRET_KEY`),
   `LITESTREAM_ACCESS_KEY_ID`, `LITESTREAM_SECRET_ACCESS_KEY` or
   `AWS_SHARED_CREDENTIALS_FILE` is set. See below.
3. Refuses to start if `AWS_CONFIG_FILE` no longer points at `/etc/aws/config`,
   `AWS_SDK_LOAD_CONFIG` is empty, or `AWS_PROFILE` names a profile other than
   `default`. Any of those would stop the SDK from ever running the helper.
4. Runs the helper once before `litestream restore`, discarding the credentials and
   logging only their `Expiration`. A wrong URL or token fails here with
   `storage credentials preflight failed`, not as an opaque credential error inside
   Litestream.

**Why the AWS_* keys must not be set in process mode.** The SDK's credential chain
tries the environment before the shared config file, and Litestream copies
`LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY` into `AWS_*` when it
starts. With any of them present the helper never runs: the node would replicate with
the static keys, look healthy, and stop replicating when those keys expired or were
revoked, with nothing at boot to say why. So the entrypoint treats the combination as a
configuration error.

### Static mode (local bench, VPS)

Chosen when `NODE_STORAGE_CREDENTIALS_URL` is unset. Keys come from
`LITESTREAM_ACCESS_KEY_ID` / `LITESTREAM_SECRET_ACCESS_KEY` or the `AWS_*` variables
and are read once, as before process mode existed. The entrypoint unsets the image's
`AWS_CONFIG_FILE` and `AWS_SDK_LOAD_CONFIG` in this mode so the SDK behaves exactly as
it did before the image carried a config file. An operator who sets `AWS_CONFIG_FILE`
to a different file keeps it. The log line lists which key variables were present, by
name only.

## The bootstrap callback

A brand new node has no API key, and nothing outside the container can mint one: on
Cloudflare Containers there is no `docker exec`. So the node hands its first key out
itself, once, on the only boot where it has a database nobody has ever held a key for.

Set `NODE_BOOTSTRAP_CALLBACK_URL` and `NODE_BOOTSTRAP_TOKEN` alongside
`NODE_BOOTSTRAP=1` and the bootstrap branch of step 3 will:

1. Run `envoy apikey:create all`. The CLI opens the store, which creates and migrates
   `/data/trisa.db`, so this works before `envoy serve` has ever run. The key lands in
   the database Litestream then replicates, so it survives every later restore.
2. Parse the `client id:` and `client secret:` lines out of the command's output.
3. `POST` `{"slug": "<LITESTREAM_PATH>", "client_id": …, "client_secret": …}` as JSON
   to the callback URL with `Authorization: Bearer $NODE_BOOTSTRAP_TOKEN`. One attempt
   and five retries, backing off 2, 4, 8, 16 and 32 seconds.
4. Continue to step 4 only if the callback was accepted. **If it never succeeds the
   container exits 1**: a node whose only API key is held by nobody cannot be driven,
   and leaving it running would put an unreachable database in the bucket under this
   slug. Delete the prefix and provision again.

The secret is never logged, never passed as a command argument and never left on disk:
the request body goes to a 0600 file under `/run/node` and the bearer token to a curl
config file, and both are removed before step 4. Only the client id is logged.

Leave `NODE_BOOTSTRAP_CALLBACK_URL` unset and nothing changes: the node boots exactly
as it did before and you mint the key by hand, as the local bench does.

The callback URL must be reachable from inside the container, which on Cloudflare
Containers means the node's own public hostname (outbound internet is on by default)
and on the bench means the host. The image's `curl` exists for this and nothing else.

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
- Process mode with a missing token, an `AWS_*` or `LITESTREAM_*` key also set, or a
  failed credentials preflight: exit 1, before Litestream runs. See
  [Storage credentials](#storage-credentials).
- `NODE_BOOTSTRAP_CALLBACK_URL` is set and the callback fails every attempt: exit 1.
  The node would otherwise serve, and replicate, a database whose only API key was
  printed into a log line nobody read.

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
| `NODE_STORAGE_CREDENTIALS_URL` | none | Selects process mode. The Worker's `<public_base>/_storage-credentials` endpoint. See [Storage credentials](#storage-credentials). |
| `NODE_STORAGE_CREDENTIALS_TOKEN` | with the URL | Bearer token for that endpoint. Missing while the URL is set: exit 1. |
| `LITESTREAM_ACCESS_KEY_ID` | static mode only | Read by Litestream itself, never named in `litestream.yml`. Set in process mode: exit 1. |
| `LITESTREAM_SECRET_ACCESS_KEY` | static mode only | As above. |
| `NODE_BOOTSTRAP` | `0` | `1` allows the node to start with an empty prefix. First boot only. |

### Bootstrap callback

Both are read on the bootstrap path only and ignored on every other boot.

| Variable | Required | What it is |
| --- | --- | --- |
| `NODE_BOOTSTRAP_CALLBACK_URL` | no | Where to POST the node's first API key, from inside the container. Unset means the old behaviour: no key is minted and no request is made. Set it and a failed callback is fatal. See [The bootstrap callback](#the-bootstrap-callback). |
| `NODE_BOOTSTRAP_TOKEN` | with the URL | Sent as `Authorization: Bearer`. A per-bootstrap random value, so a leaked one authorises nothing after the node is up. Missing while the URL is set: exit 1. |

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


`NODE_SLUG` (optional): the slug sent in the bootstrap callback body. Defaults to `LITESTREAM_PATH`, which may carry a deployment prefix such as `prod/<slug>`; a provisioner that keys nodes by bare slug should set it.
