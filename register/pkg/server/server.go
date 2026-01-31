package server

import (
	"context"
	"sync"

	"github.com/anthdm/hollywood/actor"
	"github.com/go-logr/logr"
)

type RegisterServer struct {
	log logr.Logger

	e *actor.Engine

	agents map[string]*actor.PID // key: node hostname
}

func NewRegisterServer(log logr.Logger) (*RegisterServer, error) {
	e, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		return nil, err
	}

	return &RegisterServer{
		log,
		e,
		make(map[string]*actor.PID),
	}, nil
}

func (rs *RegisterServer) Shutdown(ctx context.Context) error {
	var wg sync.WaitGroup

	for node, pid := range rs.agents {
		wg.Add(1)
		rs.log.Info("stopping actor", "node", node, "pid", pid)

		poisonCtx := rs.e.PoisonCtx(ctx, pid)

		go func(node string, poisonCtx context.Context) {
			defer wg.Done()
			<-poisonCtx.Done()
			rs.log.Info("stopped actor", "node", node)
		}(node, poisonCtx)
	}

	// Wait for all agents to stop or context timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		rs.log.Info("all agent actors stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
