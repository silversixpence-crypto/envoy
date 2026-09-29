#!/bin/sh
#
# AWS credential_process helper for the hosted Envoy node.
#
# Installed at /usr/local/bin/storage-credentials and named in /etc/aws/config. The AWS
# SDK inside Litestream runs it at boot and again whenever the Expiration it returned
# has passed, so the Worker can rotate the node's scoped R2 credentials without
# restarting the container.
#
#   GET $NODE_STORAGE_CREDENTIALS_URL
#   Authorization: Bearer $NODE_STORAGE_CREDENTIALS_TOKEN
#
# A 200 carries exactly the credential_process JSON
# ({"Version":1,"AccessKeyId","SecretAccessKey","SessionToken","Expiration"}), which is
# printed to stdout unchanged. Anything else exits 1 with a short message on stderr.
#
# The token is read from the environment at call time and handed to curl through a
# 0600 config file, never as an argument, so it never shows up in /proc/*/cmdline or
# `ps`. Neither the token nor the response body is ever written to stderr.
#
# See docker/hosted/README.md.

set -eu

die() {
    echo "storage-credentials: $*" >&2
    exit 1
}

URL="${NODE_STORAGE_CREDENTIALS_URL-}"
TOKEN="${NODE_STORAGE_CREDENTIALS_TOKEN-}"

[ -n "$URL" ] || die "NODE_STORAGE_CREDENTIALS_URL is not set"
[ -n "$TOKEN" ] || die "NODE_STORAGE_CREDENTIALS_TOKEN is not set"

# The token is written into a curl config file between double quotes, so refuse
# anything that could break out of them. The contract is 64 hex characters.
case "$TOKEN" in
    *[!0-9A-Za-z._~+/=-]*) die "NODE_STORAGE_CREDENTIALS_TOKEN contains characters that are not allowed" ;;
esac

command -v curl > /dev/null 2>&1 || die "curl is not installed"

# Everything written below holds either the token or the credentials.
umask 077

_dir="$(mktemp -d "${TMPDIR:-/tmp}/storage-credentials.XXXXXX")" || die "could not create a temporary directory"
trap 'rm -rf "$_dir"' EXIT
trap 'exit 1' HUP INT TERM

_curlrc="$_dir/curlrc"
_body="$_dir/body"

{
    echo "header = \"Authorization: Bearer ${TOKEN}\""
    echo "header = \"Accept: application/json\""
    echo "silent"
    echo "connect-timeout = 5"
    echo "max-time = 15"
    echo "output = \"${_body}\""
    echo "write-out = \"%{http_code}\""
} > "$_curlrc"

# No --fail and no -L: only a 200 counts, and a redirect is a failure, not something to
# follow with the bearer token attached. curl's own error text is dropped because the
# exit code says enough and the message is written here instead.
_rc=0
_code="$(curl -K "$_curlrc" "$URL" 2>/dev/null)" || _rc=$?

if [ "$_rc" -ne 0 ]; then
    case "$_rc" in
        6) _why="could not resolve host" ;;
        7) _why="could not connect" ;;
        28) _why="timed out" ;;
        35|51|58|60) _why="TLS failure" ;;
        *) _why="curl exit $_rc" ;;
    esac

    die "request to $URL failed: $_why"
fi

[ "$_code" = "200" ] || die "request to $URL answered HTTP ${_code:-none}"

# The SDK would reject a malformed document too, but with a far less readable error
# buried in Litestream's log. Check the shape before handing it over.
grep -Eq '"Version"[[:space:]]*:[[:space:]]*1([^0-9]|$)' "$_body" \
    || die "response from $URL is not credential_process JSON (no \"Version\":1)"
grep -Eq '"AccessKeyId"[[:space:]]*:[[:space:]]*"[^"]+"' "$_body" \
    || die "response from $URL is not credential_process JSON (no AccessKeyId)"
grep -Eq '"SecretAccessKey"[[:space:]]*:[[:space:]]*"[^"]+"' "$_body" \
    || die "response from $URL is not credential_process JSON (no SecretAccessKey)"

cat "$_body"
