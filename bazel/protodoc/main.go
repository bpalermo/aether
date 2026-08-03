// Command protodoc is the protoc plugin that renders aether's protobuf API
// reference for aethermesh.dev (//website, the `/api/` section). It is wired up
// by //bazel/protodoc:defs.bzl and never run by hand.
//
// It is a thin wrapper around github.com/pseudomuto/protoc-gen-doc rather than
// that module's own cmd/protoc-gen-doc, for two reasons.
//
// The first is that cmd/protoc-gen-doc is unbuildable here: its main package
// blank-imports three option renderers — google_api_http, lyft_validate and
// validator_field — and the module behind lyft_validate,
// github.com/lyft/protoc-gen-validate, no longer exists on GitHub, so it cannot
// be resolved at all. None of the three would render anything for aether's
// protos anyway: the only custom options they carry are buf.validate, which no
// upstream renderer knows and which the pages therefore show as raw option text
// (the /api/ page headers say so).
//
// The second is editions. Every aether proto is edition 2023 or 2024, and protoc
// refuses to invoke a code generator that does not declare editions support —
// upstream protoc-gen-doc, last released in 2021, does not. The declaration
// below is the whole fix: protoc-gen-doc reads names, types and comments, none
// of which editions changes, so the rendered reference is correct. The maximum
// is pinned to the newest edition the repository actually uses rather than left
// open, so that moving api/** to a newer edition fails this action loudly and
// the rendering gets re-checked instead of silently degrading.
package main

import (
	"log"

	gendoc "github.com/pseudomuto/protoc-gen-doc"
	"github.com/pseudomuto/protokit"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// editionsAware answers protoc's editions handshake on behalf of the wrapped
// generator.
type editionsAware struct {
	inner protokit.Plugin
}

func (p editionsAware) Generate(
	req *pluginpb.CodeGeneratorRequest,
) (*pluginpb.CodeGeneratorResponse, error) {
	resp, err := p.inner.Generate(req)
	if err != nil {
		return nil, err
	}
	resp.SupportedFeatures = proto.Uint64(uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS))
	resp.MinimumEdition = proto.Int32(int32(descriptorpb.Edition_EDITION_PROTO2))
	resp.MaximumEdition = proto.Int32(int32(descriptorpb.Edition_EDITION_2024))
	return resp, nil
}

func main() {
	if err := protokit.RunPlugin(editionsAware{inner: new(gendoc.Plugin)}); err != nil {
		log.Fatal(err)
	}
}
