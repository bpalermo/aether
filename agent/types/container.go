// Package types provides type definitions used throughout the Aether agent.
package types

// ContainerID represents a unique container identifier as a string.
type ContainerID string

// String returns the string representation of the ContainerID.
func (c ContainerID) String() string {
	return string(c)
}
