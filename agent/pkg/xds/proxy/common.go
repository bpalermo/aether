package proxy

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func typedConfig(config proto.Message) *anypb.Any {
	typedConfig, _ := anypb.New(config)
	return typedConfig
}
