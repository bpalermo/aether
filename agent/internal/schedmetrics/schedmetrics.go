// Package schedmetrics exports the node agent's own kernel scheduler counters
// as OpenTelemetry instruments under aether.agent.sched.* (issue #1131).
//
// One agent per generation was seen burning 2–8× its peers' CPU while doing the
// same work, all of it Go-scheduler wake/idle overhead. Two explanations fit and
// the profile cannot separate them: (a) more wakeups (timer phases that stop
// coinciding), or (b) costlier wakeups (the runtime's threads landing on
// deep-idle CPUs). These counters can:
//
//   - wakeups/s                  = rate(timeslices)            — (a) moves this
//   - run-queue wait per wakeup  = rate(run_delay)/rate(timeslices) — (b) moves this
//   - migrations per wakeup      = rate(migrations)/rate(timeslices) — and this
//   - CPU per wakeup             = rate(cpu_time)/rate(timeslices)
//
// The instruments are observable: procfs is read inside the collection
// callback, so they add no timer and no wakeup of their own.
package schedmetrics

import (
	"context"
	"fmt"
	"time"

	"aethermesh.dev/common/procsched"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MeterName identifies this instrumentation scope in metric backends.
const MeterName = "aether/agent-sched"

// attrSwitch labels aether.agent.sched.context_switches: "voluntary" (the
// thread blocked or slept) or "involuntary" (it was preempted).
const attrSwitch = attribute.Key("aether.sched.switch")

var (
	voluntary   = metric.WithAttributeSet(attribute.NewSet(attrSwitch.String("voluntary")))
	involuntary = metric.WithAttributeSet(attribute.NewSet(attrSwitch.String("involuntary")))
)

type instruments struct {
	switches   metric.Int64ObservableCounter
	timeslices metric.Int64ObservableCounter
	migrations metric.Int64ObservableCounter
	runDelay   metric.Float64ObservableCounter
	cpuTime    metric.Float64ObservableCounter
	threads    metric.Int64ObservableGauge
}

func newInstruments(meter metric.Meter) (*instruments, error) {
	var (
		in  instruments
		err error
	)
	if in.switches, err = meter.Int64ObservableCounter("aether.agent.sched.context_switches",
		metric.WithDescription("Context switches of the agent's threads, by aether.sched.switch (voluntary: blocked or slept; involuntary: preempted). Summed over every thread the agent has had."),
		metric.WithUnit("{switch}")); err != nil {
		return nil, err
	}
	if in.timeslices, err = meter.Int64ObservableCounter("aether.agent.sched.timeslices",
		metric.WithDescription("Times the agent's threads were put on a CPU (schedstat field 3): one per wakeup plus one per preemption that got the thread back. Its rate is the agent's wakeup rate."),
		metric.WithUnit("{timeslice}")); err != nil {
		return nil, err
	}
	if in.migrations, err = meter.Int64ObservableCounter("aether.agent.sched.migrations",
		metric.WithDescription("Times the agent's threads were moved to another CPU (se.nr_migrations). Absent on a kernel without CONFIG_SCHED_DEBUG."),
		metric.WithUnit("{migration}")); err != nil {
		return nil, err
	}
	if in.runDelay, err = meter.Float64ObservableCounter("aether.agent.sched.run_delay",
		metric.WithDescription("Time the agent's threads spent runnable but waiting for a CPU (schedstat field 2)."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if in.cpuTime, err = meter.Float64ObservableCounter("aether.agent.sched.cpu_time",
		metric.WithDescription("Time the agent's threads spent on a CPU (schedstat field 1)."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if in.threads, err = meter.Int64ObservableGauge("aether.agent.sched.threads",
		metric.WithDescription("The agent's live OS threads at the last reading."),
		metric.WithUnit("{thread}")); err != nil {
		return nil, err
	}
	return &in, nil
}

// clampInt64 converts a kernel counter for an Int64 instrument. The counters
// cannot realistically reach 2^63; clamping keeps the conversion lint-clean
// and monotonic if one ever did.
func clampInt64(v uint64) int64 {
	const maxInt64 = 1<<63 - 1
	if v > maxInt64 {
		return maxInt64
	}
	return int64(v)
}

func seconds(ns uint64) float64 {
	return time.Duration(clampInt64(ns)).Seconds()
}

// Register registers the instruments on meter and the callback that reads
// reader on every collection. Readings are folded into monotonic totals, so a
// thread that exits never makes a counter go backwards. When procfs cannot be
// read the callback reports nothing (an absent series, never a false zero).
func Register(meter metric.Meter, reader procsched.Reader) (metric.Registration, error) {
	in, err := newInstruments(meter)
	if err != nil {
		return nil, fmt.Errorf("registering scheduler instruments: %w", err)
	}
	acc := &procsched.Accumulator{}
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		threads, readErr := reader.Threads()
		if readErr != nil {
			// Best-effort: an unreadable procfs is an absent series, not a
			// collection error that would also fail every other instrument.
			return nil
		}
		t := acc.Update(threads)
		o.ObserveInt64(in.switches, clampInt64(t.Voluntary), voluntary)
		o.ObserveInt64(in.switches, clampInt64(t.Involuntary), involuntary)
		o.ObserveInt64(in.timeslices, clampInt64(t.Timeslices))
		if t.HasMigrations {
			o.ObserveInt64(in.migrations, clampInt64(t.Migrations))
		}
		o.ObserveFloat64(in.runDelay, seconds(t.RunDelayNs))
		o.ObserveFloat64(in.cpuTime, seconds(t.CPUNs))
		o.ObserveInt64(in.threads, int64(t.Threads))
		return nil
	}, in.switches, in.timeslices, in.migrations, in.runDelay, in.cpuTime, in.threads)
}
