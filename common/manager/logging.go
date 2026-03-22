package manager

import (
	"github.com/bpalermo/aether/log"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupLogging creates a logger with the given name and configures controller-runtime to use it.
func SetupLogging(debug bool, name string) logr.Logger {
	l := log.NewLogger(debug).WithName(name)
	ctrl.SetLogger(l)
	return l
}
