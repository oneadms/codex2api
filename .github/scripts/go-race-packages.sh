#!/usr/bin/env bash
# Print the `go test` selector args for one test-race shard so heavy packages
# do not share a 2-core runner.
#
# Shard sizing (measured locally under -race, 2026-09-04, after the SQLite
# schema-template fast path removed the ~1.2s per-test migration cost):
#   proxy ≈ 95s total, split by test-name initial (^Test[A-G] ≈ 53%, the rest
#   ≈ 47%; the two regexes are complementary so no test can be silently
#   skipped); admin ≈ 60s; database ≈ 50s; promptfilter ≈ 40s once its
#   CPU-bound single-threaded fixtures skip under -race; `rest` is everything
#   else. Keep each shard well under the 12m go-test timeout on CI runners.
set -euo pipefail

shard="${1:-}"
case "$shard" in
  proxy-a-g)
    echo "-run ^Test[A-G] ./proxy/..."
    ;;
  proxy-h-z)
    echo "-run ^Test[^A-G] ./proxy/..."
    ;;
  admin)
    echo ./admin
    ;;
  database)
    echo ./database
    ;;
  promptfilter)
    echo ./security/promptfilter
    ;;
  rest)
    go list ./... | grep -Ev '/(admin|database)($|/)' | grep -Ev '/proxy($|/)' | grep -Ev '/security/promptfilter$'
    ;;
  *)
    echo "unknown test-race shard: ${shard}" >&2
    exit 1
    ;;
esac
