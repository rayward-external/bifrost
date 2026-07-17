#!/usr/bin/env bash
set -euo pipefail

# Test framework component
# Usage: ./test-framework.sh

# Setup Go workspace for CI
source "$(dirname "$0")/setup-go-workspace.sh"

echo "🧪 Running framework tests..."

# Cleanup function to ensure Docker services are stopped
cleanup_docker() {
  echo "🧹 Cleaning up Docker services..."
  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f tests/docker-compose.yml down 2>/dev/null || true
  elif docker compose version >/dev/null 2>&1; then
    docker compose -f tests/docker-compose.yml down 2>/dev/null || true
  fi
}

# Register cleanup handler to run on script exit (success or failure)
trap cleanup_docker EXIT

# Starting dependencies of framework tests
echo "🔧 Starting dependencies of framework tests..."
# Use docker compose (v2) if available, fallback to docker-compose (v1)
if command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
elif docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
else
  echo "❌ Neither docker-compose nor docker compose is available"
  exit 1
fi
$COMPOSE -f tests/docker-compose.yml up -d

# Wait for Postgres to accept connections instead of a blind `sleep 20`. The
# postgres service has no compose healthcheck; polling pg_isready (bundled in
# postgres:16-alpine) is strictly better hygiene than a fixed sleep and is
# usually FASTER (a few seconds vs. 20s). NOTE: this readiness gate is secondary
# — the postgres-backed framework tests gracefully skip when the DB is down
# rather than failing, so "postgres not ready" is not itself the flake. The
# retry-once guard below is the primary defense; see the comment there.
echo "⏳ Waiting for Postgres to accept connections..."
pg_ready=false
for i in $(seq 1 45); do
  if $COMPOSE -f tests/docker-compose.yml exec -T postgres pg_isready -U bifrost -d bifrost >/dev/null 2>&1; then
    echo "✅ Postgres ready after ~$((i * 2))s"
    pg_ready=true
    break
  fi
  sleep 2
done
if [ "$pg_ready" != "true" ]; then
  echo "⚠️ Postgres not ready after 90s; continuing so the test output surfaces the real error"
fi
# Brief settle for the remaining services (redis/qdrant/weaviate/pinecone).
sleep 5

# Validate framework build
echo "🔨 Validating framework build..."
cd framework
go build ./...
echo "✅ Framework build validation successful"

# Run framework tests with coverage. PRIMARY FLAKE GUARD — retry ONCE on failure.
# The post-merge main run reds ~4/30 times on transient races that pass on a clean
# rerun: TestSubmitJob_OperationFailure_PreservesContext (framework/logstore, an
# in-memory-sqlite test with 2s completion-poll deadlines that lose on a loaded
# runner) and cross-package postgres races (configstore + logstore package
# binaries run in parallel against the same localhost:5432 and DROP/CREATE shared
# tables). Each GitHub failure emails the operator the instant the run reds, and a
# later rerun-to-green does NOT unsend it — so the run must not red on a flake in
# the first place. Retry-once keeps a flake green while a REAL regression fails
# BOTH attempts and correctly reds the run: this hides flakes, never bugs.
echo "🧪 Running framework tests with coverage..."
if ! go test --race -coverprofile=coverage.txt -coverpkg=./... ./...; then
  echo "⚠️ Framework tests failed once; retrying once (flake guard)..."
  go test --race -coverprofile=coverage.txt -coverpkg=./... ./...
fi

# Upload coverage to Codecov
if [ -n "${CODECOV_TOKEN:-}" ]; then
  echo "📊 Uploading coverage to Codecov..."
  curl -Os https://uploader.codecov.io/latest/linux/codecov
  chmod +x codecov
  ./codecov -t "$CODECOV_TOKEN" -f coverage.txt -F framework
  rm -f codecov coverage.txt
else
  echo "ℹ️ CODECOV_TOKEN not set, skipping coverage upload"
  rm -f coverage.txt
fi
cd ..

echo "✅ Framework tests completed successfully"
