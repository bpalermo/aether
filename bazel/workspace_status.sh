# Variables without STABLE_ prefix are volatile.
# These variables are not included in the cache key.
# If their values changes, a target may still include
# a stale value from a previous build.
echo "STABLE_GIT_VERSION $(git describe --tags --always --long --dirty --abbrev=40 2>/dev/null || echo 'unknown')"
# STABLE_ on purpose (#837). This is what every published image carries as
# `org.opencontainers.image.revision`, so it MUST invalidate the image config
# when the commit changes -- a volatile key would let a cached config blob
# re-publish the PREVIOUS commit's sha, which is a confidently wrong provenance
# answer rather than a missing one. Being stable costs nothing: the value is
# identical for every build at a given commit, so the remote cache still hits
# across machines, and only the (tiny) config/manifest actions re-run between
# commits -- never a layer, a compile or a link.
echo "STABLE_GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "BUILD_TIMESTAMP $(date +%s)"
echo "GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_BRANCH $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_DIRTY $(if git diff --quiet 2>/dev/null; then echo 'clean'; else echo 'dirty'; fi)"
