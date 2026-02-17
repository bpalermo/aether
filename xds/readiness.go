package xds

import "net/http"

func (s *Server) HealthzCheck(req *http.Request) error {
	s.Log.V(2).Info("liveness check called")
	return nil
}

func (s *Server) ReadyzCheck(req *http.Request) error {
	s.Log.V(2).Info("readiness check called")
	return nil
}
