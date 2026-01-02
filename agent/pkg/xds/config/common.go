package config

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TypedConfig(config proto.Message) *anypb.Any {
	c, _ := anypb.New(config)
	return c
}
