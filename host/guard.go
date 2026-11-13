package host

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type portConn struct {
	cc       grpc.ClientConnInterface
	deadline time.Duration
	slots    chan struct{}
	maxBytes int
}

var _ grpc.ClientConnInterface = (*portConn)(nil)

func (p *portConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if m, ok := args.(proto.Message); ok && proto.Size(m) > p.maxBytes {
		return status.Errorf(codes.ResourceExhausted, "request of %d bytes exceeds the %d byte limit", proto.Size(m), p.maxBytes)
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return status.Error(codes.ResourceExhausted, "plugin bulkhead is full")
	}
	defer func() { <-p.slots }()
	ctx, cancel := context.WithTimeout(ctx, p.deadline)
	defer cancel()
	return p.cc.Invoke(ctx, method, args, reply, append(opts, p.sizes()...)...)
}

func (p *portConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return p.cc.NewStream(ctx, desc, method, append(opts, p.sizes()...)...)
}

func (p *portConn) sizes() []grpc.CallOption {
	return []grpc.CallOption{grpc.MaxCallRecvMsgSize(p.maxBytes), grpc.MaxCallSendMsgSize(p.maxBytes)}
}
