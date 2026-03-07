package xds

import "net/http"

// HealthzCheck is a liveness probe handler for health check endpoints.
// It always returns nil and can be used by external health check systems to verify
// that the server process is running.
func (s *Server) HealthzCheck(req *http.Request) error {
	s.Log.V(2).Info("liveness check called")
	return nil
}

// ReadyzCheck is a readiness probe handler for health check endpoints.
// It always returns nil and can be used by external readiness check systems to verify
// that the server is ready to accept client requests.
func (s *Server) ReadyzCheck(req *http.Request) error {
	s.Log.V(2).Info("readiness check called")
	return nil
}
