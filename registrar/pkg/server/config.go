package server

type RegisterServerConfig struct {
	Network string
	Address string
}

func NewRegisterServerConfig() *RegisterServerConfig {
	return &RegisterServerConfig{
		Network: "tcp",
		Address: ":50051",
	}
}
