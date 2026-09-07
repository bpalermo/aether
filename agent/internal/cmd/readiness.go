package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	commonlog "aethermesh.dev/common/log"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// readyzAdder is the one thing agentReadiness needs from the manager, narrow so
// the aggregate is testable without a live manager (and an apiserver).
type readyzAdder interface {
	AddReadyzCheck(name string, check healthz.Checker) error
}

// agentReadiness owns the agent's /readyz checks so that ONE verdict serves two
// consumers that must never disagree: the kubelet, which polls the endpoint, and
// the node taint remover, which asks in-process.
//
// The split is what issue #740's finding 2 was: the controller's node-taint
// guard armed aether.io/agent-not-ready 30s after an identity-less agent went
// NotReady, and this agent's own taint remover — gated on the CNI socket and
// conflist chaining only (#667) — dropped it ~50ms later, every 30s for eight
// cycles. The node was tainted for about 50ms per 30s, i.e. never, and the
// escalation the readiness gate exists to trigger did not happen. Removing a
// taint is a claim that this node can mesh a new pod; readiness is where that
// claim is computed, so the remover has to read it rather than a subset of it.
//
// It also gives an operator the failure REASON. controller-runtime's
// /readyz?verbose redacts checker errors as "reason withheld", so the useful
// text ("no SPIRE SVID after 2m10s (socket …)") was unreachable from the
// endpoint an operator would actually curl. Each check is wrapped so every
// transition between passing and failing is logged exactly once.
type agentReadiness struct {
	log    *slog.Logger
	checks []*namedCheck
}

// namedCheck is one registered readiness check plus the last verdict observed
// for it, so transitions can be logged once instead of on every poll.
type namedCheck struct {
	name    string
	check   healthz.Checker
	failing atomic.Bool
}

// newAgentReadiness returns an empty aggregate.
func newAgentReadiness(log *slog.Logger) *agentReadiness {
	return &agentReadiness{log: commonlog.Named(log, "readiness")}
}

// add registers check under name with the manager and keeps it in the aggregate.
// The registered checker is the wrapped one, so the transition logging fires on
// the kubelet's polls as well as on the taint remover's in-process calls.
func (a *agentReadiness) add(m readyzAdder, name string, check healthz.Checker) error {
	nc := &namedCheck{name: name, check: check}
	a.checks = append(a.checks, nc)
	if err := m.AddReadyzCheck(name, a.wrap(nc)); err != nil {
		return fmt.Errorf("failed to set up the %s ready check: %w", name, err)
	}
	return nil
}

// wrap returns nc's checker with once-per-transition logging attached.
func (a *agentReadiness) wrap(nc *namedCheck) healthz.Checker {
	return func(req *http.Request) error {
		err := nc.check(req)
		a.logTransition(nc, err)
		return err
	}
}

// logTransition logs a check's first failure and its first subsequent pass, and
// nothing in between. The message carries the check name so an operator can grep
// for the one they saw in /readyz?verbose ("spire-svid readiness failing").
func (a *agentReadiness) logTransition(nc *namedCheck, err error) {
	if nc.failing.Swap(err != nil) == (err != nil) {
		return // no transition
	}
	if err != nil {
		a.log.Warn(nc.name+" readiness failing", "check", nc.name, "reason", err.Error())
		return
	}
	a.log.Info(nc.name + " readiness passing")
}

// Err returns the aggregate verdict — nil when every check passes, otherwise the
// first failure, named. It is the same computation /readyz performs, minus the
// HTTP.
//
// The checks registered here ignore the request (they read a Unix socket, an
// in-memory conflist observation and an SVID holder), so a nil request is what
// they get; the aggregate never accepts a check from outside this file.
func (a *agentReadiness) Err() error {
	var errs []error
	for _, nc := range a.checks {
		if err := a.wrap(nc)(nil); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", nc.name, err))
		}
	}
	return errors.Join(errs...)
}
