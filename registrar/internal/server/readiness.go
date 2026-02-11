package server

import "net/http"

func (rs *RegistrarServer) HealthzCheck(req *http.Request) error {
	rs.log.V(2).Info("liveness check called")
	return nil
}

func (rs *RegistrarServer) ReadyzCheck(req *http.Request) error {
	rs.log.V(2).Info("readiness check called")
	return nil
}
