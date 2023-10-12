package hrpc

import (
	"context"
	"github.com/tsuna/gohbase/pb"
	"google.golang.org/protobuf/proto"
)

func NewCreateNamespace(ctx context.Context, namespace string,
	options ...func(createNamespace *CreateNamespace)) *CreateNamespace {
	return &CreateNamespace{
		name: namespace,
	}
}

// CreateNamespace represents a CreateNamespace HBase call
type CreateNamespace struct {
	base
	name string
}

func (cn CreateNamespace) Name() string {
	return "CreateNamespace"
}

func (cn CreateNamespace) ToProto() proto.Message {
	return &pb.CreateNamespaceRequest{
		NamespaceDescriptor: &pb.NamespaceDescriptor{
			Name: []byte(cn.name),
		},
	}
}

func (cn CreateNamespace) NewResponse() proto.Message {
	return &pb.CreateNamespaceResponse{}
}

func (cn CreateNamespace) Description() string {
	return cn.Name()
}
