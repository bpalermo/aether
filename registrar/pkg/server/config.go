package server

const (
	defaultClusterName = "unknown"
)

type RegistrarServerConfig struct {
	Network string
	Address string

	ClusterName string
}

func NewRegisterServerConfig() *RegistrarServerConfig {
	return &RegistrarServerConfig{
		Network:     "tcp",
		Address:     ":50051",
		ClusterName: defaultClusterName,
	}
}
