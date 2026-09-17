#!/bin/sh
#
# Hosted Envoy node entrypoint.
#
#   1. materialise the node's secrets from the environment into /run/node
#   2. restore /data/trisa.db from the Litestream replica (fail closed)
#   3. refuse to start on a blank database unless NODE_BOOTSTRAP=1
#   3b. on the bootstrap path only, mint the first API key and POST it to
#       NODE_BOOTSTRAP_CALLBACK_URL (fail closed)
#   4. exec litestream replicate -exec "envoy serve"
#
# Every failure mode here is fail-closed on purpose: a node that starts on an empty
# database when the replica holds its history would silently lose every transfer it
# ever recorded, and would then replicate that empty database back over the history.
#
# See docker/hosted/README.md for the full environment.

set -eu

log() {
    echo "[hosted-entrypoint] $*"
}

die() {
    echo "[hosted-entrypoint] FATAL: $*" >&2
    exit 1
}

SECRETS_DIR=/run/node
DB_PATH=/data/trisa.db
LITESTREAM_CONFIG=/etc/litestream.yml

# ---------------------------------------------------------------------------
# 1. Secrets
# ---------------------------------------------------------------------------

# Write $2 (base64) to the file $1 with mode 0600.
write_b64() {
    _path="$1"
    _value="$2"

    printf '%s' "$_value" | base64 -d > "$_path" || die "could not base64-decode the secret for $_path"
    chmod 0600 "$_path"

    [ -s "$_path" ] || die "the secret written to $_path is empty"
}

# A ULID is 26 Crockford base32 characters (the alphabet excludes I, L, O and U).
is_ulid() {
    printf '%s' "$1" | grep -Eq '^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{26}$'
}

mkdir -p "$SECRETS_DIR" "$(dirname "$DB_PATH")"
chmod 0700 "$SECRETS_DIR"

NODE_CERTS_B64="${NODE_CERTS_B64-}"
NODE_POOL_B64="${NODE_POOL_B64-}"
NODE_JWT_KEY_B64="${NODE_JWT_KEY_B64-}"
NODE_JWT_KEY_ID="${NODE_JWT_KEY_ID-}"

if [ -z "$NODE_CERTS_B64" ] && [ -z "$NODE_POOL_B64" ] && [ -z "$NODE_JWT_KEY_B64" ] && [ -z "${TRISA_NODE_CERTS-}" ]; then
    die "no node secrets: set NODE_CERTS_B64/NODE_POOL_B64/NODE_JWT_KEY_B64, or bind-mount the files and set TRISA_NODE_CERTS/TRISA_NODE_POOL/TRISA_WEB_AUTH_KEYS yourself"
fi

if [ -n "$NODE_CERTS_B64" ]; then
    write_b64 "$SECRETS_DIR/node.pem.gz" "$NODE_CERTS_B64"
    log "wrote $SECRETS_DIR/node.pem.gz ($(wc -c < "$SECRETS_DIR/node.pem.gz") bytes)"
fi

if [ -n "$NODE_POOL_B64" ]; then
    write_b64 "$SECRETS_DIR/pool.pem.gz" "$NODE_POOL_B64"
    log "wrote $SECRETS_DIR/pool.pem.gz ($(wc -c < "$SECRETS_DIR/pool.pem.gz") bytes)"
fi

if [ -n "$NODE_JWT_KEY_B64" ]; then
    [ -n "$NODE_JWT_KEY_ID" ] || die "NODE_JWT_KEY_B64 is set but NODE_JWT_KEY_ID is not"
    is_ulid "$NODE_JWT_KEY_ID" || die "NODE_JWT_KEY_ID must be a ULID (26 Crockford base32 characters), got '$NODE_JWT_KEY_ID'"

    write_b64 "$SECRETS_DIR/jwt.pem" "$NODE_JWT_KEY_B64"

    head -n 1 "$SECRETS_DIR/jwt.pem" | grep -q -- "-----BEGIN .*PRIVATE KEY-----" \
        || die "NODE_JWT_KEY_B64 did not decode to a PEM private key"

    log "wrote $SECRETS_DIR/jwt.pem (key id $NODE_JWT_KEY_ID)"
fi

# The operator wins: a VPS may bind-mount the files and set these itself.
if [ -z "${TRISA_NODE_CERTS-}" ] && [ -f "$SECRETS_DIR/node.pem.gz" ]; then
    TRISA_NODE_CERTS="$SECRETS_DIR/node.pem.gz"
    export TRISA_NODE_CERTS
fi

if [ -z "${TRISA_NODE_POOL-}" ] && [ -f "$SECRETS_DIR/pool.pem.gz" ]; then
    TRISA_NODE_POOL="$SECRETS_DIR/pool.pem.gz"
    export TRISA_NODE_POOL
fi

if [ -z "${TRISA_TRP_CERTS-}" ] && [ -f "$SECRETS_DIR/node.pem.gz" ]; then
    TRISA_TRP_CERTS="$SECRETS_DIR/node.pem.gz"
    export TRISA_TRP_CERTS
fi

if [ -z "${TRISA_TRP_POOL-}" ] && [ -f "$SECRETS_DIR/pool.pem.gz" ]; then
    TRISA_TRP_POOL="$SECRETS_DIR/pool.pem.gz"
    export TRISA_TRP_POOL
fi

if [ -z "${TRISA_WEB_AUTH_KEYS-}" ] && [ -f "$SECRETS_DIR/jwt.pem" ]; then
    TRISA_WEB_AUTH_KEYS="${NODE_JWT_KEY_ID}:${SECRETS_DIR}/jwt.pem"
    export TRISA_WEB_AUTH_KEYS
fi

# The signing key is not optional: without TRISA_WEB_AUTH_KEYS Envoy generates a
# volatile RSA key at boot, and every container replacement would then invalidate the
# gateway's access and refresh tokens even though the database restored perfectly.
[ -n "${TRISA_WEB_AUTH_KEYS-}" ] || die "no JWT signing key: set NODE_JWT_KEY_B64 and NODE_JWT_KEY_ID, or bind-mount a key and set TRISA_WEB_AUTH_KEYS"

# `docker exec` inherits the container's configured environment, not what this script
# exported, so `envoy apikey:create` inside a running node would fail with "specify
# certificates path". Leave the exports on disk so an operator can source them:
#
#   docker exec <node> sh -c '. /run/node/env && exec envoy apikey:create all'
{
    echo "export TRISA_NODE_CERTS='${TRISA_NODE_CERTS-}'"
    echo "export TRISA_NODE_POOL='${TRISA_NODE_POOL-}'"
    echo "export TRISA_TRP_CERTS='${TRISA_TRP_CERTS-}'"
    echo "export TRISA_TRP_POOL='${TRISA_TRP_POOL-}'"
    echo "export TRISA_WEB_AUTH_KEYS='${TRISA_WEB_AUTH_KEYS-}'"
} > "$SECRETS_DIR/env"

chmod 0600 "$SECRETS_DIR/env"

log "step 1/4 secrets ready: TRISA_NODE_CERTS=${TRISA_NODE_CERTS-unset} TRISA_WEB_AUTH_KEYS=${TRISA_WEB_AUTH_KEYS-unset}"

# ---------------------------------------------------------------------------
# 2. Restore
# ---------------------------------------------------------------------------

# Litestream expands ${VAR} in the config but has no :- default syntax, so the default
# is applied here instead.
LITESTREAM_REGION="${LITESTREAM_REGION:-us-east-1}"
export LITESTREAM_REGION

[ -n "${LITESTREAM_BUCKET-}" ] || die "LITESTREAM_BUCKET is required"
[ -n "${LITESTREAM_PATH-}" ] || die "LITESTREAM_PATH is required"
[ -n "${LITESTREAM_ENDPOINT-}" ] || die "LITESTREAM_ENDPOINT is required"

# A bootstrap that was interrupted between minting the API key and delivering it
# leaves a database on disk that nobody can drive and that was never replicated
# (litestream only starts in step 4). The marker below is written before the key is
# minted and removed after the callback succeeds; if it is still here on a later boot
# with a persistent disk, that database is discarded so this boot bootstraps again.
BOOTSTRAP_MARKER="$(dirname "$DB_PATH")/.bootstrap-pending"

if [ -f "$BOOTSTRAP_MARKER" ]; then
    log "step 2/4 found an interrupted bootstrap (${BOOTSTRAP_MARKER}); discarding the undelivered database"
    rm -f "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm" "$BOOTSTRAP_MARKER"
fi

db_existed=no
[ -f "$DB_PATH" ] && db_existed=yes

log "step 2/4 restore: bucket=${LITESTREAM_BUCKET} path=${LITESTREAM_PATH} endpoint=${LITESTREAM_ENDPOINT} region=${LITESTREAM_REGION} db_existed=${db_existed}"

# /proc/uptime is monotonic; the wall clock is not (the container syncs its clock while
# the restore runs, which produced negative durations from date +%s%N).
restore_start=$(awk '{ printf "%.0f", $1 * 1000 }' /proc/uptime)

# -if-db-not-exists  exit 0 when /data/trisa.db is already there (VPS with a volume).
# -if-replica-exists exit 0 when the prefix holds no backups yet (first boot).
# Any other non-zero exit means the replica exists but could not be restored: stop,
# never fall through to a blank database.
if ! litestream restore -config "$LITESTREAM_CONFIG" -if-db-not-exists -if-replica-exists "$DB_PATH"; then
    die "litestream restore failed; refusing to start on a blank database while a replica exists"
fi

restore_end=$(awk '{ printf "%.0f", $1 * 1000 }' /proc/uptime)
restore_ms=$(( restore_end - restore_start ))

log "restore_ms=${restore_ms}"

# ---------------------------------------------------------------------------
# 3b. Bootstrap callback (function; called from the bootstrap branch of step 3)
# ---------------------------------------------------------------------------

# Hand the node's first API key to whoever provisioned it.
#
# On Cloudflare Containers there is no `docker exec`, so the key that the local bench
# mints by hand has to leave the container on its own. `envoy apikey:create all` opens
# the store, which creates and migrates /data/trisa.db, so it works before `envoy
# serve` has ever run; the key it writes is then part of the database Litestream
# replicates, and it survives every later restore.
#
# Fail closed: if the callback never succeeds, exit 1. A node whose only API key is
# held by nobody is a node nobody can drive, and it would still be replicating a
# database into the bucket under this slug.
#
# The secret is never logged and never appears in a process argument list: the request
# body goes to a 0600 file and the bearer token to a curl config file, both deleted
# before this function returns.
bootstrap_callback() {
    [ -n "${NODE_BOOTSTRAP_TOKEN-}" ] || die "NODE_BOOTSTRAP_CALLBACK_URL is set but NODE_BOOTSTRAP_TOKEN is not"

    command -v curl > /dev/null 2>&1 || die "curl is not installed in this image but NODE_BOOTSTRAP_CALLBACK_URL is set"

    _out="$SECRETS_DIR/apikey.out"
    _body="$SECRETS_DIR/bootstrap.json"
    _curlrc="$SECRETS_DIR/bootstrap.curlrc"

    # Anything this function writes holds the API secret.
    umask 077

    log "step 3b/4 minting the first API key (envoy apikey:create all)"

    : > "$BOOTSTRAP_MARKER"

    if ! /usr/local/bin/envoy apikey:create all > "$_out" 2>&1; then
        # The failure output cannot contain a key (there is none), but redact anyway.
        sed -e 's/^\([Cc]lient [Ss]ecret:\).*/\1 <redacted>/' "$_out" >&2 || true
        rm -f "$_out"

        die "apikey:create failed; the node has no API key and nothing to call back with"
    fi

    # Same two lines api/src/provision.js parseApiKey() reads: `client id:\t<value>`
    # and `client secret:\t<value>`, among envoy's own log lines on stdout.
    _client_id="$(sed -n 's/^[Cc]lient [Ii][Dd]:[[:space:]]*//p' "$_out" | tail -n 1)"
    _client_secret="$(sed -n 's/^[Cc]lient [Ss]ecret:[[:space:]]*//p' "$_out" | tail -n 1)"

    rm -f "$_out"

    [ -n "$_client_id" ] || die "could not parse a client id out of apikey:create"
    [ -n "$_client_secret" ] || die "could not parse a client secret out of apikey:create"

    # The JSON below is assembled by hand, so refuse anything that would need escaping
    # rather than emitting a broken body. Envoy's keys are base62.
    case "$_client_id$_client_secret" in
        *[!0-9A-Za-z_.-]*) die "apikey:create returned characters this entrypoint will not embed in JSON" ;;
    esac

    log "step 3b/4 minted api key client_id=${_client_id} (secret withheld)"

    printf '{"slug":"%s","client_id":"%s","client_secret":"%s"}\n' \
        "${NODE_SLUG:-$LITESTREAM_PATH}" "$_client_id" "$_client_secret" > "$_body"

    # -K keeps the bearer token and the body out of /proc/*/cmdline.
    {
        echo "request = \"POST\""
        echo "header = \"Authorization: Bearer ${NODE_BOOTSTRAP_TOKEN}\""
        echo "header = \"Content-Type: application/json\""
        echo "data-binary = \"@${_body}\""
        echo "silent"
        echo "show-error"
        echo "max-time = 30"
        echo "output = \"/dev/null\""
        echo "write-out = \"%{http_code}\""
    } > "$_curlrc"

    # One attempt plus five retries, backing off 2, 4, 8, 16, 32 seconds: a minute of
    # patience for a Worker that is still coming up, and a bounded one.
    _attempt=1
    _attempts=6
    _backoff=2
    _ok=0

    # Only a 2xx counts. `--fail` alone would let a redirect through as success (curl
    # does not follow it here, and 3xx is not an error to --fail), so the status code
    # is checked explicitly instead.
    while :; do
        _code="$(curl -K "$_curlrc" "$NODE_BOOTSTRAP_CALLBACK_URL" 2>/dev/null || true)"

        case "$_code" in
            2[0-9][0-9])
                _ok=1
                break
                ;;
        esac

        [ "$_attempt" -lt "$_attempts" ] || break

        log "step 3b/4 callback attempt ${_attempt}/${_attempts} to ${NODE_BOOTSTRAP_CALLBACK_URL} failed (http ${_code:-none}); retrying in ${_backoff}s"

        sleep "$_backoff"

        _attempt=$(( _attempt + 1 ))
        _backoff=$(( _backoff * 2 ))
    done

    rm -f "$_body" "$_curlrc"

    umask 022

    if [ "$_ok" != 1 ]; then
        # apikey:create has already created the database, and nothing has replicated it
        # yet (litestream starts in step 4). Leaving it on disk would make the next boot
        # of this container, or of a VPS with a volume, take the existing-database path
        # and serve a node whose only key was never delivered. Remove it so the next
        # boot is a clean bootstrap again.
        rm -f "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm" "$BOOTSTRAP_MARKER"

        die "bootstrap callback to ${NODE_BOOTSTRAP_CALLBACK_URL} failed ${_attempts} times; database discarded, refusing to serve a node whose API key nobody holds"
    fi

    rm -f "$BOOTSTRAP_MARKER"

    log "step 3b/4 callback accepted by ${NODE_BOOTSTRAP_CALLBACK_URL}"
}

# ---------------------------------------------------------------------------
# 3. Bootstrap gate
# ---------------------------------------------------------------------------

if [ ! -f "$DB_PATH" ]; then
    if [ "${NODE_BOOTSTRAP-}" != "1" ]; then
        die "no replica for this node and NODE_BOOTSTRAP is not set"
    fi

    log "step 3/4 path=bootstrap (empty ${LITESTREAM_PATH} prefix, NODE_BOOTSTRAP=1; envoy will create and migrate a new database)"

    if [ -n "${NODE_BOOTSTRAP_CALLBACK_URL-}" ]; then
        bootstrap_callback
    else
        log "step 3b/4 skipped: NODE_BOOTSTRAP_CALLBACK_URL is not set, mint the API key yourself (docker exec, see README)"
    fi
elif [ "$db_existed" = "yes" ]; then
    log "step 3/4 path=existing-volume (${DB_PATH} was already present, $(wc -c < "$DB_PATH") bytes; restore skipped)"
else
    log "step 3/4 path=restored (${DB_PATH} restored from ${LITESTREAM_BUCKET}/${LITESTREAM_PATH} in ${restore_ms} ms, $(wc -c < "$DB_PATH") bytes)"
fi

# ---------------------------------------------------------------------------
# 4. Replicate and supervise
# ---------------------------------------------------------------------------

log "step 4/4 exec litestream replicate -exec 'envoy serve'"

exec litestream replicate -config "$LITESTREAM_CONFIG" -exec "/usr/local/bin/envoy serve"
