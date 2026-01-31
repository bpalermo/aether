package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/anthdm/hollywood/actor"
	"github.com/anthdm/hollywood/remote"
	"github.com/go-logr/logr"
)

type RegisterServer struct {
	log logr.Logger

	e *actor.Engine

	remote *remote.Remote

	clients map[string]*actor.PID // key: address value: *pid
	agents  map[string]string     // key: address value: hostname
}

var _ actor.Receiver = (*RegisterServer)(nil)

func NewRegisterServer(cfg *RegisterServerConfig, log logr.Logger) (*RegisterServer, error) {
	rem := remote.New(fmt.Sprintf(":%d", cfg.Port), remote.NewConfig())
	e, err := actor.NewEngine(actor.NewEngineConfig().WithRemote(rem))
	if err != nil {
		return nil, err
	}

	return &RegisterServer{
		log.WithName("register-server"),
		e,
		rem,
		make(map[string]*actor.PID),
		make(map[string]string),
	}, nil
}

func (rs *RegisterServer) Start() {
	rs.e.Spawn(func() actor.Receiver { return rs }, "server", actor.WithID("primary"))
}

func (rs *RegisterServer) Shutdown(ctx context.Context) error {
	var wg sync.WaitGroup

	for node, pid := range rs.clients {
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
		// Wait for the remote to fully stop
		rs.remote.Stop().Wait()
		rs.log.Info("remote stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
