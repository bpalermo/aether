package procsched

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeThread lays out one fake /proc/<pid>/task/<tid> directory. An empty
// sched leaves the file out (a kernel without CONFIG_SCHED_DEBUG).
func writeThread(t *testing.T, root, tid, schedstat, status, sched string) {
	t.Helper()
	dir := filepath.Join(root, "42", "task", tid)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedstat"), []byte(schedstat), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644))
	if sched != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sched"), []byte(sched), 0o644))
	}
}

func statusOf(vol, invol int) string {
	return "Name:\taether-agent\nState:\tS (sleeping)\nvoluntary_ctxt_switches:\t" +
		strconv.Itoa(vol) + "\nnonvoluntary_ctxt_switches:\t" + strconv.Itoa(invol) + "\n"
}

const sched = `aether-agent (42, #threads: 2)
-------------------------------------------------------------------
se.exec_start                                :      123456789.123456
se.nr_migrations                             :                   17
nr_switches                                  :                  300
`

func TestReaderThreads(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "42", "1000 200 30\n", statusOf(25, 5), sched)
	writeThread(t, root, "43", "500 100 10\n", statusOf(8, 2), "")
	// Not a thread directory: ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "42", "task", "x"), 0o755))

	got, err := Reader{Root: root, PID: "42"}.Threads()
	require.NoError(t, err)
	assert.Equal(t, map[int]ThreadCounters{
		42: {CPUNs: 1000, RunDelayNs: 200, Timeslices: 30, Voluntary: 25, Involuntary: 5, Migrations: 17, HasMigrations: true},
		43: {CPUNs: 500, RunDelayNs: 100, Timeslices: 10, Voluntary: 8, Involuntary: 2},
	}, got)
}

// TestParseSchedstat pins the one parser of the kernel's
// "<cpu_ns> <runq_ns> <timeslices>" line. The agent's scheduler metrics and the
// proxy supervisor's stall sampler both read through it.
func TestParseSchedstat(t *testing.T) {
	cpu, runq, slices, err := ParseSchedstat([]byte("807472818 798485462 3329\n"))
	require.NoError(t, err)
	assert.EqualValues(t, 807472818, cpu)
	assert.EqualValues(t, 798485462, runq)
	assert.EqualValues(t, 3329, slices)

	// What the kernel writes with scheduler accounting off.
	cpu, runq, slices, err = ParseSchedstat([]byte("0 0 0\n"))
	require.NoError(t, err)
	assert.Zero(t, cpu+runq+slices)

	for _, bad := range []string{"", "1", "1 2", "x 1 1", "1 y 1", "1 1 z", "-1 1 1"} {
		_, _, _, err := ParseSchedstat([]byte(bad))
		assert.Error(t, err, "%q", bad)
	}
}

// TestReaderThreadIDs: the numeric entries of the task directory, whatever
// their files hold, and os.ErrNotExist for a process that is gone.
func TestReaderThreadIDs(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "42", "1000 200 30\n", statusOf(1, 1), "")
	writeThread(t, root, "44", "garbage\n", statusOf(1, 1), "")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "42", "task", "x"), 0o755))

	tids, err := Reader{Root: root, PID: "42"}.ThreadIDs()
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{42, 44}, tids)

	_, err = Reader{Root: root, PID: "43"}.ThreadIDs()
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReaderSkipsUnreadableThread(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "42", "1000 200 30\n", statusOf(1, 1), "")
	writeThread(t, root, "44", "garbage\n", statusOf(1, 1), "")

	got, err := Reader{Root: root, PID: "42"}.Threads()
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Contains(t, got, 42)
}

func TestReaderNoProcfs(t *testing.T) {
	err := Reader{Root: t.TempDir(), PID: "42"}.Probe()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupported))
}

func TestAccumulatorIsMonotonicAcrossThreadExit(t *testing.T) {
	var a Accumulator
	got := a.Update(map[int]ThreadCounters{
		1: {CPUNs: 100, RunDelayNs: 10, Timeslices: 5, Voluntary: 4, Involuntary: 1},
		2: {CPUNs: 50, RunDelayNs: 5, Timeslices: 2, Voluntary: 2},
	})
	assert.Equal(t, Totals{CPUNs: 150, RunDelayNs: 15, Timeslices: 7, Voluntary: 6, Involuntary: 1, Threads: 2}, got)

	// Thread 2 exits; thread 1 grows. Thread 2's past counts stay in the total.
	got = a.Update(map[int]ThreadCounters{
		1: {CPUNs: 130, RunDelayNs: 12, Timeslices: 8, Voluntary: 6, Involuntary: 2},
	})
	assert.Equal(t, Totals{CPUNs: 180, RunDelayNs: 17, Timeslices: 10, Voluntary: 8, Involuntary: 2, Threads: 1}, got)

	// TID 2 reused by a new thread whose counters restart below the old ones.
	got = a.Update(map[int]ThreadCounters{
		1: {CPUNs: 130, RunDelayNs: 12, Timeslices: 8, Voluntary: 6, Involuntary: 2},
		2: {CPUNs: 3, RunDelayNs: 1, Timeslices: 1, Voluntary: 1, Migrations: 4, HasMigrations: true},
	})
	assert.Equal(t, Totals{CPUNs: 183, RunDelayNs: 18, Timeslices: 11, Voluntary: 9, Involuntary: 2, Migrations: 4, HasMigrations: true, Threads: 2}, got)
}

// TestReaderSelf reads the test process itself: on Linux every Go process has
// several threads, and they have run.
func TestReaderSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	threads, err := Reader{}.Threads()
	require.NoError(t, err)
	require.NotEmpty(t, threads)
	var a Accumulator
	tot := a.Update(threads)
	assert.Positive(t, tot.CPUNs)
	assert.Positive(t, tot.Timeslices)
	assert.Equal(t, len(threads), tot.Threads)
}
