#!/bin/sh
# End-to-end smoke test against a running instance. It exercises the behaviour
# the task describes — status codes, error format, edge cases — and the cache
# invalidation path, because those are the things a reviewer has to trust.
#
#   ./hack/smoke.sh                     # against http://127.0.0.1:8080
#   BASE=http://localhost:9090 ./hack/smoke.sh
set -eu

BASE=${BASE:-http://127.0.0.1:8080}
# The script is safe to run repeatedly against the same database: everything it
# creates uses a number this run picked at random, so it cannot collide with the
# leftovers of an earlier run.
NUM=$((1000 + RANDOM % 9000))
BODY=$(mktemp)
trap 'rm -f "$BODY"' EXIT

pass=0
fail=0

# request <name> <expected status> <method> <path> <curl args...>
request() {
	name=$1
	want=$2
	shift 2
	code=$(curl -sS -o "$BODY" -w '%{http_code}' -X "$1" "$BASE$2" "${@:3}" 2>/dev/null || echo 000)
	if [ "$code" = "$want" ]; then
		printf '  ok   %-52s %s\n' "$name" "$code"
		pass=$((pass + 1))
	else
		printf '  FAIL %-52s got %s want %s\n' "$name" "$code" "$want"
		fail=$((fail + 1))
		[ -s "$BODY" ] && sed 's/^/         /' "$BODY"
	fi
}

# expect_in <name> <substring>
expect_in() {
	if grep -q "$2" "$BODY"; then
		printf '  ok   %-52s contains %s\n' "$1" "$2"
		pass=$((pass + 1))
	else
		printf '  FAIL %-52s missing %s\n' "$1" "$2"
		fail=$((fail + 1))
		sed 's/^/         /' "$BODY"
	fi
}

count() { # count <name> <jq-less substring count>
	n=$(grep -o "$2" "$BODY" | wc -l | tr -d ' ')
	if [ "$n" = "$3" ]; then
		printf '  ok   %-52s %s x %s\n' "$1" "$3" "$2"
		pass=$((pass + 1))
	else
		printf '  FAIL %-52s %s occurrences, want %s\n' "$1" "$n" "$3"
		fail=$((fail + 1))
		sed 's/^/         /' "$BODY"
	fi
}

VIEWER='viewer:viewer12345'
ADMIN='admin:admin12345'
JSON='-H Content-Type:application/json'

echo "probing $BASE"

echo "health"
request "GET /healthz" 200 GET /healthz
request "GET /readyz" 200 GET /readyz
request "GET /nope is a JSON 404" 404 GET /nope

echo "authentication"
request "no credentials" 401 GET /api/v1/channels
 WWW=$(curl -sS -o /dev/null -D - "$BASE/api/v1/channels" | grep -i '^www-authenticate' | tr -d '\r')
if echo "$WWW" | grep -qi 'Basic realm='; then
	printf '  ok   %-52s %s\n' "WWW-Authenticate header" "$WWW"
	pass=$((pass + 1))
else
	printf '  FAIL %-52s %s\n' "WWW-Authenticate header" "missing"
	fail=$((fail + 1))
fi
request "wrong password" 401 GET /api/v1/channels -u viewer:nope-nope-nope
request "inactive user" 401 GET /api/v1/channels -u blocked:blocked12345
request "viewer list" 200 GET /api/v1/channels -u "$VIEWER"
expect_in "list shape" '"channels"'
expect_in "list has expires_at" '"expires_at"'

echo "conditional requests"
ETAG=$(curl -sS -D - -o /dev/null -u "$VIEWER" "$BASE/api/v1/channels" | grep -i '^etag' | tr -d '\r' | cut -d' ' -f2)
request "If-None-Match" 304 GET /api/v1/channels -u "$VIEWER" -H "If-None-Match: $ETAG"

echo "playback"
request "known channel" 200 GET /api/v1/channels/1/playback -u "$VIEWER"
expect_in "signed link" 'sig='
expect_in "hls path" 'index.m3u8'
request "no subscription" 403 GET /api/v1/channels/5/playback -u "$VIEWER"
expect_in "403 code" 'subscription_required'
request "switched off channel" 404 GET /api/v1/channels/6/playback -u "$VIEWER"
request "unknown channel" 404 GET /api/v1/channels/99999/playback -u "$VIEWER"
request "non numeric id" 400 GET /api/v1/channels/abc/playback -u "$VIEWER"
request "zero id" 400 GET /api/v1/channels/0/playback -u "$VIEWER"
request "negative id" 400 GET /api/v1/channels/-3/playback -u "$VIEWER"
request "viewer on admin api" 403 GET /api/v1/admin/users -u "$VIEWER"

echo "admin reads"
request "admin users" 200 GET /api/v1/admin/users -u "$ADMIN"
count "four demo users" '"id":' 4
if grep -q 'password_hash' "$BODY"; then
	printf '  FAIL %-52s password hash leaked\n' "no hash in response"
	fail=$((fail + 1))
else
	printf '  ok   %-52s\n' "no hash in response"
	pass=$((pass + 1))
fi
request "admin channels" 200 GET /api/v1/admin/channels -u "$ADMIN"
request "admin cache stats" 200 GET /api/v1/admin/cache -u "$ADMIN"
request "admin sees everything" 200 GET /api/v1/channels -u "$ADMIN"
request "admin playback of ungranted channel" 200 GET /api/v1/channels/5/playback -u "$ADMIN"
request "all channels view" 200 GET "/api/v1/channels?all=true" -u "$VIEWER"
expect_in "unavailable channel listed" '"available":false'

echo "admin writes and cache invalidation"
request "viewer cannot see ch5" 403 GET /api/v1/channels/5/playback -u "$VIEWER"
request "grant ch5 to viewer" 200 PUT /api/v1/admin/users/2/subscriptions/5 -u "$ADMIN" $JSON -d '{"expires_at":null}'
request "viewer gets ch5 link right after grant" 200 GET /api/v1/channels/5/playback -u "$VIEWER"
request "revoke ch5" 204 DELETE /api/v1/admin/users/2/subscriptions/5 -u "$ADMIN"
request "viewer loses ch5 right after revoke" 403 GET /api/v1/channels/5/playback -u "$VIEWER"
request "unknown field rejected" 400 POST /api/v1/admin/users -u "$ADMIN" $JSON -d '{"login":"x","password":"whatever12","admim":true}'
request "short password rejected" 400 POST /api/v1/admin/users -u "$ADMIN" $JSON -d '{"login":"shorty","password":"short"}'
request "bad stream path rejected" 400 POST /api/v1/admin/channels -u "$ADMIN" $JSON -d "{\"number\": $NUM, \"name\": \"Test\", \"stream_path\": \"live/..\"}"
request "create channel" 201 POST /api/v1/admin/channels -u "$ADMIN" $JSON -d "{\"number\": $NUM, \"name\": \"Смоук тест\", \"stream_path\": \"/live/ch$NUM/index.m3u8\"}"
NEW_ID=$(grep -o '"id":[0-9]*' "$BODY" | head -1 | cut -d: -f2)
request "duplicate number" 409 POST /api/v1/admin/channels -u "$ADMIN" $JSON -d "{\"number\": $NUM, \"name\": \"Дубль\", \"stream_path\": \"/live/ch$NUM/index.m3u8\"}"
request "switch channel off" 200 PATCH "/api/v1/admin/channels/$NEW_ID" -u "$ADMIN" $JSON -d '{"is_active":false}'
request "off channel is 404 for admin" 404 GET "/api/v1/channels/$NEW_ID/playback" -u "$ADMIN"
request "unknown channel id" 404 PATCH /api/v1/admin/channels/424242 -u "$ADMIN" $JSON -d '{"is_active":false}'

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
