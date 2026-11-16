// Package a2apb holds the descriptors of the A2A 1.0 gRPC service, for
// relaying it with dynamic messages (docs/a2a.md).
//
// a2a.proto is the protocol's definition, from the specification
// directory of github.com/a2aproject/A2A (Apache-2.0). a2a.binpb is its
// compiled FileDescriptorProto. To rebuild it after replacing a2a.proto:
//
//	cd internal/a2apb/gen && go run . .. ../a2a.binpb
//
// The generator is a module of its own, so the compiler it uses is not
// a dependency of the gateway.
package a2apb

import (
	_ "embed"
	"fmt"
	"sync"

	// The files a2a.proto imports are resolved from the registry.
	_ "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "google.golang.org/protobuf/types/known/emptypb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
)

//go:embed a2a.binpb
var compiled []byte

// ServiceName is the full name of the A2A service.
const ServiceName = "lf.a2a.v1.A2AService"

// Service returns the descriptor of the A2A service.
var Service = sync.OnceValues(func() (protoreflect.ServiceDescriptor, error) {
	var fdp descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal(compiled, &fdp); err != nil {
		return nil, fmt.Errorf("a2a.binpb: %w", err)
	}
	fd, err := protodesc.NewFile(&fdp, protoregistry.GlobalFiles)
	if err != nil {
		return nil, fmt.Errorf("a2a.binpb: %w", err)
	}
	svc := fd.Services().ByName("A2AService")
	if svc == nil {
		return nil, fmt.Errorf("a2a.binpb has no A2AService")
	}
	return svc, nil
})
