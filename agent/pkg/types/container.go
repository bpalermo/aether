package types

type ContainerID string

func (c ContainerID) String() string {
	return string(c)
}
