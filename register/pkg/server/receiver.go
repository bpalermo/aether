package server

import (
	"github.com/anthdm/hollywood/actor"
	registerv1 "github.com/bpalermo/aether/api/aether/register/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

func (rs *RegisterServer) Receive(ctx *actor.Context) {
	switch msg := ctx.Message().(type) {
	case *registryv1.Endpoint:
		rs.handleEndpoint(ctx)
	case *registerv1.Disconnect:
		cAddr := ctx.Sender().GetAddress()
		pid, ok := rs.clients[cAddr]
		if !ok {
			rs.log.Info("unknown client disconnected", "client", pid.Address)
			return
		}
		hostname, ok := rs.agents[cAddr]
		if !ok {
			rs.log.Info("unknown user disconnected", "client", pid.Address)
			return
		}
		rs.log.Info("client disconnected", "agent", hostname)
		delete(rs.clients, cAddr)
		delete(rs.agents, cAddr)
	case *registerv1.Connect:
		cAddr := ctx.Sender().GetAddress()
		if _, ok := rs.clients[cAddr]; ok {
			rs.log.Info("client already connected", "client", ctx.Sender().GetID())
			return
		}
		if _, ok := rs.agents[cAddr]; ok {
			rs.log.Info("user already connected", "client", ctx.Sender().GetID())
			return
		}
		rs.clients[cAddr] = ctx.Sender()
		rs.agents[cAddr] = msg.NodeHostname
		rs.log.Info("new agent connected",
			"id", ctx.Sender().GetID(), "addr", ctx.Sender().GetAddress(), "sender", ctx.Sender(),
			"hostname", msg.NodeHostname,
		)
	}
}
