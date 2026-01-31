package server

const (
	defaultRegisterServerPort uint16 = 50051
)

type RegisterServerConfig struct {
	Port uint16
}

func NewRegisterServerConfig() *RegisterServerConfig {
	return &RegisterServerConfig{
		Port: defaultRegisterServerPort,
	}
}
