package host

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type portConn struct {
	cc       grpc.ClientConnInterface
	deadline time.Duration
	idle     time.Duration
	slots    chan struct{}
	maxBytes int
}

var _ grpc.ClientConnInterface = (*portConn)(nil)

func (p *portConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if m, ok := args.(proto.Message); ok && proto.Size(m) > p.maxBytes {
		return status.Errorf(codes.ResourceExhausted, "request of %d bytes exceeds the %d byte limit", proto.Size(m), p.maxBytes)
	}
	if err := p.acquire(); err != nil {
		return err
	}
	defer p.release()
	ctx, cancel := context.WithTimeout(ctx, p.deadline)
	defer cancel()
	return p.cc.Invoke(ctx, method, args, reply, append(opts, p.sizes()...)...)
}

func (p *portConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	if err := p.acquire(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var once sync.Once
	finish := func() { once.Do(func() { cancel(); p.release() }) }
	context.AfterFunc(ctx, finish)
	cs, err := p.cc.NewStream(ctx, desc, method, append(opts, p.sizes()...)...)
	if err != nil {
		finish()
		return nil, err
	}
	return &idleStream{ClientStream: cs, cancel: finish, idle: p.idle}, nil
}

func (p *portConn) acquire() error {
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
		return status.Error(codes.ResourceExhausted, "plugin bulkhead is full")
	}
}

func (p *portConn) release() { <-p.slots }

type idleStream struct {
	grpc.ClientStream
	cancel func()
	idle   time.Duration
}

func (s *idleStream) SendMsg(m any) error {
	return s.guard(func() error { return s.ClientStream.SendMsg(m) })
}

func (s *idleStream) RecvMsg(m any) error {
	err := s.guard(func() error { return s.ClientStream.RecvMsg(m) })
	if err != nil {
		s.cancel()
	}
	return err
}

func (s *idleStream) guard(op func() error) error {
	if s.idle < 0 {
		return op()
	}
	var expired atomic.Bool
	timer := time.AfterFunc(s.idle, func() { expired.Store(true); s.cancel() })
	err := op()
	if !timer.Stop() && expired.Load() {
		return status.Errorf(codes.DeadlineExceeded, "plugin stream idle for %s", s.idle)
	}
	return err
}

func (p *portConn) sizes() []grpc.CallOption {
	return []grpc.CallOption{grpc.MaxCallRecvMsgSize(p.maxBytes), grpc.MaxCallSendMsgSize(p.maxBytes)}
}
