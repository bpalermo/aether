# Variables without STABLE_ prefix are volatile.
# These variables are not included in the cache key.
# If their values changes, a target may still include
# a stale value from a previous build.
echo "STABLE_GIT_VERSION $(git describe --tags --always --long --dirty --abbrev=40 2>/dev/null || echo 'unknown')"
# The release commit, STABLE_ so it is cache-key material: //tools/buildid stamps
# it into every released binary's GNU build-ID (#651), which is the only key the
# Pyroscope symbolizer joins samples to symbols by. It must therefore be exact
# (not derived from `git describe`) and must invalidate the stamping action when
# it changes. The volatile GIT_COMMIT below stays: rules_img uses it for tags.
echo "STABLE_GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "BUILD_TIMESTAMP $(date +%s)"
echo "GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_BRANCH $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_DIRTY $(if git diff --quiet 2>/dev/null; then echo 'clean'; else echo 'dirty'; fi)"
