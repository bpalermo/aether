package config

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func XDSConfigSourceADS() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
	}
}

func TypedConfig(config proto.Message) *anypb.Any {
	c, _ := anypb.New(config)
	return c
}
