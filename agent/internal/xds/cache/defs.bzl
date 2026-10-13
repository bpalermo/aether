"""The cache package's tests, as two targets (cache_go_test)."""

load("@rules_go//go:def.bzl", "go_test")

# The seeded random walks of proposal 043: single-goroutine, deterministic for
# a seed, and nine tenths of the suite's time (TestPerPort_SixthWalk alone is
# seven minutes under the race detector on a CI runner). One regular
# expression, used by both targets below, so that every test is in exactly one
# of them: a new walk is added HERE.
WALKS = "^(" + "|".join([
    "TestOneHolderPerNameHoldsOverARandomWalk",
    "TestPerPort_CaptureTablesMemoEqualsFreshBuild",
    "TestPerPort_ColdAndWarmAnswerAlike",
    "TestPerPort_EveryStateAndTransitionIsConsistent",
    "TestPerPort_PartlyColdAndWarmAnswerAlike",
    "TestPerPort_SixthWalk",
    "TestPerPort_SixthWalkUnscoped",
    "TestPerPort_SnapshotDoesNotDependOnRowOrder",
    "TestPerPort_Walk",
    "TestPerPort_WalkIsHistoryIndependent",
    "TestPerPort_WalkOverEveryInput",
    "TestPerPort_WalkOverEveryInput_UnscopedListing",
    "TestPerPort_WalkWithRemovalsAndReplacements",
    "TestStatus_AgreesWithTheTables_OnEveryNode",
    "TestStatus_AgreesWithTheTables_OverAHistory",
]) + ")$$"  # "$$": a test's args are subject to "Make variable" substitution

def cache_go_test(name, **kwargs):
    """go_test for this package, split in two (Gazelle writes the call: map_kind).

    <name> runs everything but the walks, and <name minus _test>_walk_test runs
    the walks and nothing else. They are one target's worth of sources and
    dependencies; the split exists for the race gate, which runs this package
    three times over (it is one of the concurrency-heavy ones, and the one race
    the detector ever caught showed in two runs of three). Repeating a seeded
    single-goroutine walk three times finds nothing the first run did not, and
    it had brought the required race job to 54 of its 60 minutes: the walk
    target is run once there (.github/workflows/ci.yaml, "race"), with every
    walk in a shard of its own. No round and no seed is cut.

    A --test_filter that names a walk must be given to the walk target: the
    other one skips it whatever the filter says.

    Args:
      name: the name Gazelle gives the package's test.
      **kwargs: the go_test attributes.
    """
    args = kwargs.pop("args", [])
    kwargs.pop("shard_count", None)
    go_test(
        name = name,
        args = args + ["-test.skip=" + WALKS],
        shard_count = 8,
        **kwargs
    )

    # One walk is one test and cannot be split further: the longest takes
    # seven minutes under the race detector on a CI runner, which is most of
    # a "long" test's fifteen. The walks get an hour.
    walk = dict(kwargs)
    walk["timeout"] = "eternal"
    go_test(
        name = name[:-len("_test")] + "_walk_test",
        args = args + ["-test.run=" + WALKS],
        shard_count = 16,
        **walk
    )
