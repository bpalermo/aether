# Variables without STABLE_ prefix are volatile.
# These variables are not included in the cache key.
# If their values changes, a target may still include
# a stale value from a previous build.
echo "BUILD_TIMESTAMP $(date +%s)"
echo "GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_BRANCH $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_DIRTY $(if git diff --quiet 2>/dev/null; then echo 'clean'; else echo 'dirty'; fi)"
