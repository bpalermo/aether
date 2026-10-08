# Variables without STABLE_ prefix are volatile.
# These variables are not included in the cache key.
# If their values changes, a target may still include
# a stale value from a previous build.
echo "STABLE_GIT_VERSION $(git describe --tags --always --long --dirty --abbrev=40 2>/dev/null || echo 'unknown')"
# STABLE_ on purpose, and DO NOT REMOVE IT (#1378). No image and no binary
# built in THIS workspace carries the commit any more (the proxy image, built
# in the nested proxy/ workspace with its own status script, still does, on
# purpose), so an unchanged image keeps its digest from one commit to the next. What still must change with every commit is what NAMES a
# build: the per-commit image tag (`dev-<sha>`, bazel/img/go_multi_arch_image.bzl)
# and the charts' `X.Y.Z-<sha>` version. Those are written by actions that take
# stable-status.txt as an input, and a change to a STABLE_ key is the only thing
# in this file that re-runs an action. Without a commit-derived STABLE_ key a
# warm Bazel server re-uses the previous commit's result and names commit B's
# artifacts after commit A (measured for the image tag: a status whose only
# commit-derived key was the volatile GIT_COMMIT below kept the first commit's
# `dev-<sha>` across a second commit). STABLE_GIT_VERSION above also changes
# with the commit today; this key is the one that says so on purpose.
#
# It costs nothing: the value is the same for every build at a commit, so the
# remote cache still hits across machines, and between commits only the push
# specs and the chart packages re-run, never a layer, a compile or a link.
# scripts/check-image-digest-stability.sh fails if this key stops being HEAD.
echo "STABLE_GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "BUILD_TIMESTAMP $(date +%s)"
echo "GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_BRANCH $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_DIRTY $(if git diff --quiet 2>/dev/null; then echo 'clean'; else echo 'dirty'; fi)"
