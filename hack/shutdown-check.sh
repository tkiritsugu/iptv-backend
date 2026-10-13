#!/bin/sh
# Check that SIGTERM stops the service correctly: the process handles the signal
# (exit code 0 rather than 143/137 from an unhandled signal or a kill after the
# grace period), it drains and logs, and nothing panics.
#
#   docker build -t iptv-backend:local . && ./hack/shutdown-check.sh
set -eu

IMAGE=${IMAGE:-iptv-backend:local}
NAME=iptv-shutdown-check
DIR=$(mktemp -d)
LOG=/tmp/iptv-shutdown-$$.log
# The container runs as its own unprivileged user; a bind mount it must write to
# has to be open regardless of which uid created it on the host.
chmod 0777 "$DIR"

cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$DIR" "$LOG"
}
trap cleanup EXIT

docker rm -f "$NAME" >/dev/null 2>&1 || true
echo "starting $IMAGE"
docker run -d --name "$NAME" -p 18081:8080 -v "$DIR:/data" \
	-e IPTV_DB=/data/iptv.db -e IPTV_LOG_FORMAT=text "$IMAGE" >/dev/null

i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18081/readyz 2>/dev/null || echo 000)" = 200 ]; do
	i=$((i + 1))
	if [ "$i" -gt 100 ]; then
		echo "FAIL: the service did not become ready"
		docker logs "$NAME"
		exit 1
	fi
	sleep 0.2
done
echo "  ok   became ready"

# Stop with a generous grace period: if the process ignored SIGTERM, docker kills
# it after 10 s and the exit code shows it.
docker stop --time 10 "$NAME" >/dev/null
docker logs "$NAME" > "$LOG" 2>&1 || true

fail=0
code=$(docker inspect --format '{{.State.ExitCode}}' "$NAME")
case "$code" in
	0) echo "  ok   exit code 0 — SIGTERM was handled" ;;
	*)
		echo "  FAIL exit code $code — the signal was not handled (143 default, 137 killed)"
		fail=1
		;;
esac

if grep -q 'shutting down' "$LOG"; then
	echo "  ok   shutdown logged"
else
	echo "  FAIL no shutdown line in the log"
	tail -5 "$LOG"
	fail=1
fi
if grep -q 'stopped' "$LOG"; then
	echo "  ok   listeners and database released before exit"
else
	echo "  FAIL the service exited before finishing its shutdown"
	fail=1
fi
if grep -qi 'panic\|DATA RACE' "$LOG"; then
	echo "  FAIL the log contains a panic or a race"
	fail=1
fi

[ "$fail" -eq 0 ] && echo "graceful shutdown ok"
exit "$fail"
