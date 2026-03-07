// Package config provides Envoy configuration helpers for xDS resources.
// It contains utility functions for building common Envoy configuration patterns
// such as ADS config sources, protocol options, and SPIRE cluster configurations.
package config

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// XDSConfigSourceADS creates a ConfigSource that uses ADS (Aggregated Discovery Service)
// for dynamic configuration updates.
func XDSConfigSourceADS() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
	}
}

// TypedConfig wraps a protobuf message as a Google Any type.
// This is used to package Envoy extension configurations for transport in xDS messages.
func TypedConfig(config proto.Message) *anypb.Any {
	c, _ := anypb.New(config)
	return c
}
