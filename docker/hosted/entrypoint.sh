#!/bin/sh
#
# Hosted Envoy node entrypoint.
#
#   1. materialise the node's secrets from the environment into /run/node
#   2. restore /data/trisa.db from the Litestream replica (fail closed)
#   3. refuse to start on a blank database unless NODE_BOOTSTRAP=1
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

db_existed=no
[ -f "$DB_PATH" ] && db_existed=yes

log "step 2/4 restore: bucket=${LITESTREAM_BUCKET} path=${LITESTREAM_PATH} endpoint=${LITESTREAM_ENDPOINT} region=${LITESTREAM_REGION} db_existed=${db_existed}"

restore_start=$(date +%s%N)

# -if-db-not-exists  exit 0 when /data/trisa.db is already there (VPS with a volume).
# -if-replica-exists exit 0 when the prefix holds no backups yet (first boot).
# Any other non-zero exit means the replica exists but could not be restored: stop,
# never fall through to a blank database.
if ! litestream restore -config "$LITESTREAM_CONFIG" -if-db-not-exists -if-replica-exists "$DB_PATH"; then
    die "litestream restore failed; refusing to start on a blank database while a replica exists"
fi

restore_end=$(date +%s%N)
restore_ms=$(( (restore_end - restore_start) / 1000000 ))

log "restore_ms=${restore_ms}"

# ---------------------------------------------------------------------------
# 3. Bootstrap gate
# ---------------------------------------------------------------------------

if [ ! -f "$DB_PATH" ]; then
    if [ "${NODE_BOOTSTRAP-}" != "1" ]; then
        die "no replica for this node and NODE_BOOTSTRAP is not set"
    fi

    log "step 3/4 path=bootstrap (empty ${LITESTREAM_PATH} prefix, NODE_BOOTSTRAP=1; envoy will create and migrate a new database)"
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
