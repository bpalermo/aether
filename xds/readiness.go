package xds

import (
	"errors"
	"net/http"
)

// HealthzCheck is a liveness probe handler for health check endpoints.
// It returns an error when the server is not live, indicating the process
// should be restarted by the container runtime.
func (s *Server) HealthzCheck(req *http.Request) error {
	s.Log.V(2).Info("liveness check called")
	if !s.liveness.Load() {
		return errors.New("server is not live")
	}
	return nil
}

// ReadyzCheck is a readiness probe handler for health check endpoints.
// It returns an error when the server is not ready to accept client requests,
// causing the pod to be removed from service endpoints until it becomes ready.
func (s *Server) ReadyzCheck(req *http.Request) error {
	s.Log.V(2).Info("readiness check called")
	if !s.readiness.Load() {
		return errors.New("server is not ready")
	}
	return nil
}
