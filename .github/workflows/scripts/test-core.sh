#!/usr/bin/env bash
set -euo pipefail

# Test core component
# Usage: ./test-core.sh

# Setup Go workspace for CI
source "$(dirname "$0")/setup-go-workspace.sh"

echo "🧪 Running core tests..."

# Build MCP test servers for STDIO tests
echo "🔧 Building MCP test servers..."
for mcp_dir in examples/mcps/*/; do
  if [ -d "$mcp_dir" ]; then
    mcp_name=$(basename "$mcp_dir")
    if [ -f "$mcp_dir/go.mod" ]; then
      echo "  Building $mcp_name (Go)..."
      mkdir -p "$mcp_dir/bin"
      pushd "$mcp_dir" > /dev/null
      GOWORK=off go build -o "bin/$mcp_name" .
      popd > /dev/null
    elif [ -f "$mcp_dir/package.json" ]; then
      echo "  Building $mcp_name (TypeScript)..."
      pushd "$mcp_dir" > /dev/null
      npm ci --silent && npm run build
      popd > /dev/null
    fi
  fi
done
echo "✅ MCP test servers built"

# Validate core build
echo "🔨 Validating core build..."
cd core
go mod download
go build ./...
echo "✅ Core build validation successful"

# Run core tests with coverage
echo "🧪 Running core tests with coverage..."
go test -race -timeout 20m -coverprofile=coverage.txt -coverpkg=./... ./...

# Upload coverage to Codecov
if [ -n "${CODECOV_TOKEN:-}" ]; then
  echo "📊 Uploading coverage to Codecov..."
  curl -Os https://uploader.codecov.io/latest/linux/codecov
  chmod +x codecov
  ./codecov -t "$CODECOV_TOKEN" -f coverage.txt -F core
  rm -f codecov coverage.txt
else
  echo "ℹ️ CODECOV_TOKEN not set, skipping coverage upload"
  rm -f coverage.txt
fi
cd ..

echo "✅ Core tests completed successfully"
