package server

type RegistrarServerConfig struct {
	Network string
	Address string
}

func NewRegisterServerConfig() *RegistrarServerConfig {
	return &RegistrarServerConfig{
		Network: "tcp",
		Address: ":50051",
	}
}
