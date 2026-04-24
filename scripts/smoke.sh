#!/usr/bin/env bash
# Smoke test: signaling + agent + shelltest end-to-end.
#
# Starts the signaling service, enrols an agent via the RFC 8628 device-code
# flow, runs the agent, then uses the headless shelltest client to open a
# WebRTC session, spawn a shell, send a command, and verify its output.
#
# Meant to run in CI on Linux. On macOS it should also work (creack/pty
# supports it); on Windows use `bash` from Git for Windows + note that the
# shell is cmd.exe.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

BIN="$ROOT/bin"
mkdir -p "$BIN"

SMOKE="$(mktemp -d)"
SIG_PID=""
AGENT_PID=""

cleanup() {
  set +e
  [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null
  [ -n "$SIG_PID"   ] && kill "$SIG_PID"   2>/dev/null
  # Give processes a tick to write logs before rm -rf eats them.
  sleep 0.2
  if [ "${CI:-}" = "true" ] && [ -d "$SMOKE" ]; then
    echo "--- signaling log ---"; cat "$SMOKE/sig.log"   2>/dev/null || true
    echo "--- agent log ---";     cat "$SMOKE/run.log"   2>/dev/null || true
    echo "--- enroll log ---";    cat "$SMOKE/enroll.log" 2>/dev/null || true
  fi
  rm -rf "$SMOKE"
}
trap cleanup EXIT

echo "== building binaries =="
go build -o "$BIN/signaling" ./cmd/signaling
go build -o "$BIN/agent"     ./cmd/agent
go build -o "$BIN/shelltest" ./cmd/shelltest

PORT=8765
SERVER="http://127.0.0.1:$PORT"

echo "== starting signaling =="
"$BIN/signaling" -addr "127.0.0.1:$PORT" -db "$SMOKE/sig.db" > "$SMOKE/sig.log" 2>&1 &
SIG_PID=$!
# Wait for the port.
for i in $(seq 1 40); do
  if curl -sSf "$SERVER/api/devices" >/dev/null 2>&1; then break; fi
  sleep 0.1
done

echo "== enrolling agent =="
export CONNECT_AGENT_CONFIG="$SMOKE/agent.json"
"$BIN/agent" enroll --server "$SERVER" > "$SMOKE/enroll.log" 2>&1 &
ENROLL_PID=$!
USER_CODE=""
for i in $(seq 1 40); do
  sleep 0.25
  if grep -q "Code:" "$SMOKE/enroll.log"; then
    USER_CODE=$(grep "Code:" "$SMOKE/enroll.log" | head -1 | awk '{print $2}')
    break
  fi
done
if [ -z "$USER_CODE" ]; then
  echo "ERROR: never saw user code in enroll log" >&2
  cat "$SMOKE/enroll.log" >&2
  exit 1
fi
echo "user_code=$USER_CODE"

curl -fsS -X POST "$SERVER/api/enroll/approve" \
  -H "Content-Type: application/json" \
  -d "{\"user_code\":\"$USER_CODE\",\"name\":\"ci-smoke\"}"
echo

wait "$ENROLL_PID"
DEVICE_ID=$(sed -n 's/.*"device_id": *"\([^"]*\)".*/\1/p' "$SMOKE/agent.json")
if [ -z "$DEVICE_ID" ]; then
  echo "ERROR: could not parse device_id from agent.json" >&2
  cat "$SMOKE/agent.json" >&2
  exit 1
fi
echo "device_id=$DEVICE_ID"

echo "== running agent =="
"$BIN/agent" run > "$SMOKE/run.log" 2>&1 &
AGENT_PID=$!
# Wait until signaling sees the device online.
for i in $(seq 1 40); do
  sleep 0.25
  if curl -sS "$SERVER/api/devices" | grep -q '"online":true'; then break; fi
done

echo "== shelltest =="
"$BIN/shelltest" \
  -server "$SERVER" \
  -device "$DEVICE_ID" \
  -cmd   "echo hello-from-shelltest" \
  -expect "hello-from-shelltest" \
  -wait  15s

echo "== smoke test passed =="
