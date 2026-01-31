package server

import "github.com/anthdm/hollywood/actor"

func (rs *RegisterServer) handleEndpoint(ctx *actor.Context) {
	for _, pid := range rs.clients {
		// don't send the endpoint to the place where it came from.
		if !pid.Equals(ctx.Sender()) {
			rs.log.Info("forwarding endpoint", "pid", pid.ID, "addr", pid.Address, "msg", ctx.Message())
			ctx.Forward(pid)
		}
	}
}
