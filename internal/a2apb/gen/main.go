package main

import (
	"context"
	"os"

	"github.com/bufbuild/protocompile"
	_ "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	_ "google.golang.org/protobuf/types/known/emptypb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
)

// Usage: go run . DIR OUT compiles DIR/a2a.proto into OUT.
func main() {
	src := &protocompile.SourceResolver{ImportPaths: []string{os.Args[1]}}
	reg := protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		return protocompile.SearchResult{Desc: fd}, nil
	})
	c := protocompile.Compiler{Resolver: protocompile.CompositeResolver{src, reg}}
	files, err := c.Compile(context.Background(), "a2a.proto")
	if err != nil {
		panic(err)
	}
	var fd protoreflect.FileDescriptor = files[0]
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(protodesc.ToFileDescriptorProto(fd))
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[2], b, 0o644); err != nil {
		panic(err)
	}
}
