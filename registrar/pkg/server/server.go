package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"buf.build/go/protovalidate"
	"github.com/anthdm/hollywood/actor"
	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
)

type RegisterServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	cfg *RegisterServerConfig

	log logr.Logger

	grpcServer *grpc.Server
	listener   net.Listener

	subscribers   map[string]registrarv1.RegistrarService_SubscribeServer
	subscribersMu sync.RWMutex
	clients       map[string]*actor.PID // key: address value: *pid
}

func NewRegisterServer(cfg *RegisterServerConfig, log logr.Logger) (*RegisterServer, error) {
	validator, _ := protovalidate.New()

	return &RegisterServer{
		registrarv1.UnimplementedRegistrarServiceServer{},
		cfg,
		log.WithName("register-server"),
		grpc.NewServer(
			grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
		),
		nil,
		make(map[string]registrarv1.RegistrarService_SubscribeServer),
		sync.RWMutex{},
		make(map[string]*actor.PID),
	}, nil
}

func (rs *RegisterServer) Start(errCh chan<- error) error {
	listener, err := net.Listen(rs.cfg.Network, rs.cfg.Address)
	if err != nil {
		rs.log.Error(err, "failed to listen", "network", rs.cfg.Network, "address", rs.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
	}
	rs.listener = listener

	registrarv1.RegisterRegistrarServiceServer(rs.grpcServer, rs)

	go func() {
		if err := rs.grpcServer.Serve(listener); err != nil {
			errCh <- err
		}
	}()

	return nil
}

// Shutdown gracefully stops the gRPC server
func (rs *RegisterServer) Shutdown(ctx context.Context) error {
	if rs.grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			rs.grpcServer.GracefulStop()
			close(stopped)
		}()

		select {
		case <-stopped:
			// Graceful stop completed
			rs.log.V(1).Info("gRPC server graceful stop completed")
		case <-ctx.Done():
			// Force stop on context timeout
			rs.log.V(1).Info("gRPC server forced stop due to timeout")
			rs.grpcServer.Stop()
			return ctx.Err()
		}
	}

	return nil
}

func (rs *RegisterServer) Subscribe(stream registrarv1.RegistrarService_SubscribeServer) (err error) {
	in, err := stream.Recv()
	if err != nil {
		return err
	}

	proxyId := in.ProxyId
	rs.log.V(1).Info("new stream received", "proxyId", proxyId)

	// Register this subscriber
	rs.subscribersMu.Lock()
	rs.subscribers[proxyId] = stream
	rs.subscribersMu.Unlock()

	// Cleanup on exit
	defer func() {
		rs.log.V(1).Info("unsubscribing", "proxyId", proxyId)
		rs.subscribersMu.Lock()
		delete(rs.subscribers, proxyId)
		rs.subscribersMu.Unlock()
		rs.log.Info("unsubscribed", "proxyId", proxyId)
	}()

	// Handle incoming messages
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rs.log.V(1).Info("incoming request", "proxyId", req.ProxyId)

		// Broadcast to all other subscribers
		rs.handleRequest(req)
	}
}

func (rs *RegisterServer) handleRequest(req *registrarv1.SubscribeRequest) {

}

func (rs *RegisterServer) broadcast(senderID string) {
	rs.subscribersMu.RLock()
	defer rs.subscribersMu.RUnlock()

	for proxyID, _ := range rs.subscribers {
		if proxyID == senderID {
			rs.log.V(1).Info("skipping sender", "proxyID", proxyID)
			continue // Do not send it to sender
		}

		rs.log.V(1).Info("sending event", "proxyID", proxyID)
		//if err := subscriberStream.Send(msg); err != nil {
		//	rs.log.Error(err, "failed to send event", "proxyID", proxyID)
		//}
	}
}
