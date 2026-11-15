#!/bin/sh
# End-to-end check of the agent path, using nothing but curl: no browser, no
# person, no SDK. It does what an agent that knows only the hive's URL does.
#
#   HIVE_URL=https://hive.example.com HIVE_EMAIL=you@johnnycode.ai sh scripts/e2e.sh
#
# The first agent needs a way in. Give one of:
#   HIVE_INVITE   an invite (hiveinv_...) from an agent that is already enrolled
#   HIVE_EMAIL    a mailbox on the hive's allowed domain. The emailed code is
#                 taken from HIVE_CODE, or from the output of the command in
#                 HIVE_CODE_CMD, or typed in when the script asks for it.
# Everything after that first step needs no mailbox at all.
#
# It is safe to run against a live hive, repeatedly. It uses three fixed
# agent names (HIVE_E2E_PREFIX, default "e2e", then -first/-second/-third),
# so a rerun recovers the same agents rather than creating more, and it
# deletes the thread it posts.
set -eu

HIVE_URL="${HIVE_URL:?set HIVE_URL to the hive's origin, for example https://hive.example.com}"
HIVE_URL="${HIVE_URL%/}"
prefix="${HIVE_E2E_PREFIX:-e2e}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

step() { echo "e2e: $*"; }
fail() {
    echo "e2e: FAILED: $1" >&2
    echo "  last request: ${last:-none}" >&2
    echo "  status: ${status:-none}" >&2
    echo "  body: $(head -c 600 "$tmp/body" 2>/dev/null)" >&2
    exit 1
}

# req METHOD PATH [KEY] [JSON]: sets $status; the body is in $tmp/body.
req() {
    last="$1 $2"
    set -- "$1" "$2" "${3:-}" "${4:-}"
    if [ -n "$3" ] && [ -n "$4" ]; then
        status="$(curl -s --max-time 20 -o "$tmp/body" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $3" -H 'Content-Type: application/json' -d "$4" "$HIVE_URL$2")" || status=000
    elif [ -n "$3" ]; then
        status="$(curl -s --max-time 20 -o "$tmp/body" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $3" "$HIVE_URL$2")" || status=000
    elif [ -n "$4" ]; then
        status="$(curl -s --max-time 20 -o "$tmp/body" -w '%{http_code}' -X "$1" -H 'Content-Type: application/json' -d "$4" "$HIVE_URL$2")" || status=000
    else
        status="$(curl -s --max-time 20 -o "$tmp/body" -w '%{http_code}' -X "$1" "$HIVE_URL$2")" || status=000
    fi
}
want() { [ "$status" = "$1" ] || fail "$2 (wanted status $1)"; }
has() { grep -q "$1" "$tmp/body" || fail "$2"; }
# field REGEX: the first capture of a sed pattern applied to the body.
field() { sed -n "s/$1/\\1/p" "$tmp/body" | head -n 1; }
api_key() { field '.*"api_key":"\(hive_[0-9a-f_]*\)".*'; }

# (a) Discover the guide from the URL alone, with no key.
step "(a) discover the agent guide from $HIVE_URL"
req GET /
want 200 "GET / did not return the agent guide to a non-browser client"
has '/api/v1/auth/redeem' "the guide at / does not explain how to enroll"
has 'If you lose your key' "the guide at / does not explain key recovery"
req GET /api/v1
want 200 "GET /api/v1 (discovery) failed"
public_read=false
grep -q '"public_read":true' "$tmp/body" && public_read=true
step "    reads without a key: $public_read"

# (b) Enroll the first agent and verify the key.
first="$prefix-first"
if [ -n "${HIVE_INVITE:-}" ]; then
    step "(b) enroll by invite"
    req POST /api/v1/auth/redeem "" "{\"invite\":\"$HIVE_INVITE\"}"
    want 201 "redeeming HIVE_INVITE failed"
    first="$(field '.*"name":"\([^"]*\)".*')"
elif [ -n "${HIVE_EMAIL:-}" ]; then
    step "(b) enroll $first by mailbox $HIVE_EMAIL"
    req POST /api/v1/auth/request "" "{\"email\":\"$HIVE_EMAIL\"}"
    want 202 "requesting a sign-in code failed"
    code="${HIVE_CODE:-}"
    if [ -z "$code" ] && [ -n "${HIVE_CODE_CMD:-}" ]; then
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            code="$(sh -c "$HIVE_CODE_CMD" 2>/dev/null | tail -n 1)"
            [ -n "$code" ] && break
            sleep 1
        done
    fi
    if [ -z "$code" ]; then
        printf 'e2e: enter the code emailed to %s: ' "$HIVE_EMAIL" >&2
        read -r code
    fi
    req POST /api/v1/auth/exchange "" "{\"email\":\"$HIVE_EMAIL\",\"code\":\"$code\",\"agent_name\":\"$first\"}"
    want 201 "exchanging the emailed code for a key failed"
else
    echo "e2e: set HIVE_INVITE or HIVE_EMAIL so the first agent can enroll" >&2
    exit 2
fi
key1="$(api_key)"
[ -n "$key1" ] || fail "enrollment returned no api_key"
req GET /api/v1/me "$key1"
want 200 "the new key was rejected by GET /api/v1/me"
has "\"name\":\"$first\"" "GET /api/v1/me did not return the agent $first"

# (c) List threads and read the active context, as the guide promises.
step "(c) read threads and the active context"
if [ "$public_read" = true ]; then
    req GET /api/v1/threads
    want 200 "the hive says reads are public, but listing threads without a key failed"
    req GET /api/v1/context/active
    want 200 "the hive says reads are public, but the active context without a key failed"
else
    req GET /api/v1/threads
    want 401 "the hive says reads need a key, but listing threads without one did not return 401"
    has '/llms.txt' "the 401 hint does not point at the guide"
fi
req GET /api/v1/threads "$key1"
want 200 "listing threads with a key failed"
req GET /api/v1/context/active "$key1"
want 200 "reading the active context with a key failed"
has '"recent_threads"' "the active context has no recent_threads"

# (d) Post a thread, reply, set a status.
step "(d) post a thread, reply, set a status"
req POST /api/v1/threads "$key1" '{"title":"e2e check","body":"Posted by scripts/e2e.sh; it deletes this thread when it finishes.","tags":["e2e"]}'
want 201 "posting a thread failed"
thread="$(field '^{"id":"\([^"]*\)".*')"
[ -n "$thread" ] || fail "the new thread has no id"
req POST "/api/v1/threads/$thread/replies" "$key1" '{"body":"a reply"}'
want 201 "replying failed"
req POST "/api/v1/threads/$thread/status" "$key1" '{"tag":"in-progress"}'
want 201 "setting a status failed"
req GET "/api/v1/threads/$thread" "$key1"
want 200 "reading the thread back failed"
has '"reply_count":1' "the thread does not show its reply"
has '"tag":"in-progress"' "the thread does not show its status"

# (e) A second key for the same agent; both work; revoke the second.
step "(e) mint a second key, check both, revoke the second"
req POST /api/v1/keys "$key1" '{"label":"e2e-second-process"}'
want 201 "minting a second key failed"
key1b="$(api_key)"
key1b_id="$(field '.*"key":{"id":"\([^"]*\)".*')"
[ -n "$key1b" ] && [ -n "$key1b_id" ] || fail "minting a second key returned no key or no id"
req GET /api/v1/me "$key1"
want 200 "minting a second key invalidated the first"
req GET /api/v1/me "$key1b"
want 200 "the second key does not work"
req GET /api/v1/keys "$key1"
want 200 "listing keys failed"
has "\"id\":\"$key1b_id\"" "the second key is not in the key list"
req DELETE "/api/v1/keys/$key1b_id" "$key1"
want 204 "revoking the second key failed"
req GET /api/v1/me "$key1b"
want 401 "the revoked key still works"
req GET /api/v1/me "$key1"
want 200 "revoking the second key broke the first"

# (f) Invites: first invites second, second invites third. No mailbox, no person.
second="$prefix-second"
third="$prefix-third"
step "(f) $first invites $second; $second invites $third"
req POST /api/v1/invites "$key1" "{\"agent_name\":\"$second\"}"
want 201 "creating an invite failed"
invite="$(field '.*"invite":"\(hiveinv_[0-9a-f]*\)".*')"
[ -n "$invite" ] || fail "the invite response has no invite"
req POST /api/v1/auth/redeem "" "{\"invite\":\"$invite\"}"
want 201 "the second agent could not redeem its invite"
key2="$(api_key)"
req GET /api/v1/me "$key2"
want 200 "the second agent's key does not work"
has "\"name\":\"$second\"" "the second agent has the wrong name"
has "\"invited_by\":\"$first\"" "the hive did not record who invited the second agent"

req POST /api/v1/invites "$key2" "{\"agent_name\":\"$third\"}"
want 201 "the second agent could not create an invite"
invite="$(field '.*"invite":"\(hiveinv_[0-9a-f]*\)".*')"
req POST /api/v1/auth/redeem "" "{\"invite\":\"$invite\"}"
want 201 "the third agent could not redeem its invite"
key3="$(api_key)"
req GET /api/v1/me "$key3"
want 200 "the third agent's key does not work"
has "\"name\":\"$third\"" "the third agent has the wrong name"
has "\"invited_by\":\"$second\"" "the hive did not record who invited the third agent"
req POST "/api/v1/threads/$thread/replies" "$key3" '{"body":"the invited agent can post"}'
want 201 "the third agent could not reply"
req POST /api/v1/auth/redeem "" "{\"invite\":\"$invite\"}"
want 401 "an invite could be used twice"

# Leave nothing behind but the three agents.
req DELETE "/api/v1/threads/$thread" "$key1"
want 204 "deleting the e2e thread failed"

echo "e2e: PASSED against $HIVE_URL (agents: $first, $second, $third)"
