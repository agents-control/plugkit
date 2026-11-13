package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// MessageHeader carries, base64url-encoded protobuf JSON, the non-bytes fields of a raw-body stream's first message.
const MessageHeader = "Acplug-Message"

const rawChunk = 64 << 10

// DeadlineHeader carries the remaining call budget in milliseconds on every HTTP/JSON mapping request.
const DeadlineHeader = "Acplug-Deadline-Ms"

var jsonOut = protojson.MarshalOptions{UseProtoNames: true}

var jsonIn = protojson.UnmarshalOptions{DiscardUnknown: true}

type httpConn struct {
	base    string
	client  *http.Client
	token   string
	maxBody int64
}

var _ grpc.ClientConnInterface = (*httpConn)(nil)

func newHTTPConn(base string, client *http.Client, token string, maxBody int64) *httpConn {
	return &httpConn{base: strings.TrimRight(base, "/"), client: client, token: token, maxBody: maxBody}
}

func (h *httpConn) request(ctx context.Context, method string, args any) (*http.Response, error) {
	msg, ok := args.(proto.Message)
	if !ok {
		return nil, status.Error(codes.Internal, "request is not a protobuf message")
	}
	body, err := jsonOut.Marshal(msg)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return h.send(ctx, http.MethodPost, method, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
}

func (h *httpConn) send(ctx context.Context, verb, method string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, verb, h.base+method, body)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	maps.Copy(req.Header, hdr)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if d, ok := ctx.Deadline(); ok {
		req.Header.Set(DeadlineHeader, strconv.FormatInt(max(time.Until(d).Milliseconds(), 0), 10))
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for k, v := range md {
			for _, x := range v {
				req.Header.Add(k, x)
			}
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.Unavailable, "plugin request failed")
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, httpStatusError(resp)
	}
	return resp, nil
}

func (h *httpConn) Invoke(ctx context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	resp, err := h.request(ctx, method, args)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBody+1))
	if err != nil {
		return status.Error(codes.Unavailable, "plugin response truncated")
	}
	if int64(len(data)) > h.maxBody {
		return status.Error(codes.ResourceExhausted, "plugin response exceeds the message size limit")
	}
	out, ok := reply.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "reply is not a protobuf message")
	}
	if err := jsonIn.Unmarshal(data, out); err != nil {
		return status.Error(codes.Internal, "plugin reply is not valid JSON for "+method)
	}
	return nil
}

func (h *httpConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	switch {
	case desc.ClientStreams && !desc.ServerStreams:
		return &httpUpload{ctx: ctx, cancel: cancel, conn: h, method: method, done: make(chan struct{})}, nil
	case desc.ServerStreams && !desc.ClientStreams:
		return &httpStream{ctx: ctx, cancel: cancel, conn: h, method: method}, nil
	}
	cancel()
	return nil, status.Error(codes.Unimplemented, "http mapping supports unary, server-streaming and client-streaming methods only")
}

func bytesField(m proto.Message) protoreflect.FieldDescriptor {
	var found protoreflect.FieldDescriptor
	fields := m.ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		if f := fields.Get(i); f.Kind() == protoreflect.BytesKind && f.Cardinality() != protoreflect.Repeated {
			if found != nil {
				return nil
			}
			found = f
		}
	}
	return found
}

func encodeMessage(m proto.Message) (string, error) {
	raw, err := jsonOut.Marshal(m)
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type httpStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *httpConn
	method string

	mu  sync.Mutex
	req proto.Message

	body     io.ReadCloser
	scan     *bufio.Reader
	raw      protoreflect.FieldDescriptor
	meta     []byte
	received bool
}

func (s *httpStream) Header() (metadata.MD, error) { return nil, nil }
func (s *httpStream) Trailer() metadata.MD         { return nil }
func (s *httpStream) Context() context.Context     { return s.ctx }
func (s *httpStream) CloseSend() error             { return nil }

func (s *httpStream) SendMsg(m any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, ok := m.(proto.Message)
	if !ok || s.req != nil {
		return status.Error(codes.Internal, "server-streaming call sends one protobuf request")
	}
	s.req = msg
	return nil
}

func (s *httpStream) open(req, reply proto.Message) error {
	if req == nil {
		return status.Error(codes.Internal, "RecvMsg before SendMsg")
	}
	s.raw = bytesField(reply)
	var resp *http.Response
	var err error
	if s.raw == nil {
		resp, err = s.conn.request(s.ctx, s.method, req)
	} else {
		var hdr string
		if hdr, err = encodeMessage(req); err == nil {
			resp, err = s.conn.send(s.ctx, http.MethodGet, s.method, nil, http.Header{MessageHeader: {hdr}})
		}
	}
	if err != nil {
		return err
	}
	if v := resp.Header.Get(MessageHeader); v != "" {
		if s.meta, err = base64.RawURLEncoding.DecodeString(v); err != nil {
			_ = resp.Body.Close()
			return status.Error(codes.Internal, "plugin message header is not base64url")
		}
	}
	s.body, s.scan = resp.Body, bufio.NewReaderSize(resp.Body, rawChunk)
	return nil
}

func (s *httpStream) RecvMsg(m any) error {
	out, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "reply is not a protobuf message")
	}
	if s.body == nil {
		s.mu.Lock()
		req := s.req
		s.mu.Unlock()
		if err := s.open(req, out); err != nil {
			s.cancel()
			return err
		}
	}
	if s.raw != nil {
		return s.recvRaw(out)
	}
	for {
		line, err := readLine(s.scan, s.conn.maxBody)
		if status.Code(err) == codes.ResourceExhausted {
			s.close()
			return err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			return decodeFrame(line, m)
		}
		if err != nil {
			return s.end(err)
		}
	}
}

func (s *httpStream) recvRaw(out proto.Message) error {
	proto.Reset(out)
	first := !s.received
	s.received = true
	if first && s.meta != nil {
		if err := jsonIn.Unmarshal(s.meta, out); err != nil {
			s.close()
			return status.Error(codes.Internal, "plugin message header is not valid JSON")
		}
	}
	buf := make([]byte, rawChunk)
	n, err := io.ReadFull(s.scan, buf)
	if n > 0 {
		out.ProtoReflect().Set(s.raw, protoreflect.ValueOfBytes(buf[:n]))
		return nil
	}
	if first && s.meta != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return s.end(err)
}

func (s *httpStream) close() {
	s.cancel()
	_ = s.body.Close()
}

func (s *httpStream) end(err error) error {
	s.close()
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if s.ctx.Err() != nil {
		return status.FromContextError(s.ctx.Err()).Err()
	}
	return status.Error(codes.Unavailable, "plugin stream broken")
}

type httpUpload struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *httpConn
	method string
	done   chan struct{}

	mu     sync.Mutex
	pw     *io.PipeWriter
	field  protoreflect.FieldDescriptor
	resp   *http.Response
	err    error
	closed bool
	read   bool
}

func (u *httpUpload) Header() (metadata.MD, error) { return nil, nil }
func (u *httpUpload) Trailer() metadata.MD         { return nil }
func (u *httpUpload) Context() context.Context     { return u.ctx }

func (u *httpUpload) start(meta string) {
	pr, pw := io.Pipe()
	u.pw = pw
	hdr := http.Header{"Content-Type": {"application/octet-stream"}}
	if meta != "" {
		hdr.Set(MessageHeader, meta)
	}
	go func() {
		defer close(u.done)
		u.resp, u.err = u.conn.send(u.ctx, http.MethodPut, u.method, pr, hdr)
		_ = pr.CloseWithError(io.ErrClosedPipe)
	}()
}

func (u *httpUpload) SendMsg(m any) error {
	msg, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "request is not a protobuf message")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return status.Error(codes.Internal, "SendMsg after CloseSend")
	}
	field := bytesField(msg)
	if field == nil {
		return status.Error(codes.Unimplemented, "http mapping streams a request only when its message has exactly one bytes field")
	}
	data := msg.ProtoReflect().Get(field).Bytes()
	rest := proto.Clone(msg)
	rest.ProtoReflect().Clear(field)
	if u.pw == nil {
		meta, err := encodeMessage(rest)
		if err != nil {
			return err
		}
		u.field = field
		u.start(meta)
	} else if proto.Size(rest) > 0 {
		return status.Error(codes.InvalidArgument, "http mapping carries request fields other than bytes in the first message only")
	}
	if _, err := u.pw.Write(data); err != nil {
		<-u.done
		if u.err != nil {
			return u.err
		}
		return status.Error(codes.Unavailable, "plugin closed the upload")
	}
	return nil
}

func (u *httpUpload) CloseSend() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.closed = true
	if u.pw == nil {
		u.start("")
	}
	return u.pw.Close()
}

func (u *httpUpload) RecvMsg(m any) error {
	if err := u.CloseSend(); err != nil {
		return err
	}
	<-u.done
	u.mu.Lock()
	defer u.mu.Unlock()
	defer u.cancel()
	if u.err != nil {
		return u.err
	}
	if u.read {
		return io.EOF
	}
	u.read = true
	defer u.resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(u.resp.Body, u.conn.maxBody+1))
	if err != nil || int64(len(data)) > u.conn.maxBody {
		return status.Error(codes.ResourceExhausted, "plugin upload reply exceeds the message size limit")
	}
	out, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "reply is not a protobuf message")
	}
	if err := jsonIn.Unmarshal(data, out); err != nil {
		return status.Error(codes.Internal, "plugin reply is not valid JSON for "+u.method)
	}
	return nil
}

func readLine(r *bufio.Reader, limit int64) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if int64(len(line)+len(chunk)) > limit {
			return nil, status.Error(codes.ResourceExhausted, "plugin stream line exceeds the message size limit")
		}
		line = append(line, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func decodeFrame(line []byte, m any) error {
	var frame struct {
		Error *errorBody `json:"error"`
	}
	if json.Unmarshal(line, &frame) == nil && frame.Error != nil {
		return frame.Error.status()
	}
	out, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "reply is not a protobuf message")
	}
	if err := jsonIn.Unmarshal(line, out); err != nil {
		return status.Error(codes.Internal, "plugin stream frame is not valid JSON")
	}
	return nil
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e errorBody) status() error {
	code, ok := codeNames[strings.ToUpper(e.Code)]
	if !ok {
		code = codes.Unknown
	}
	return status.Error(code, e.Message)
}

var codeNames = map[string]codes.Code{
	"INVALID_ARGUMENT": codes.InvalidArgument, "NOT_FOUND": codes.NotFound, "ALREADY_EXISTS": codes.AlreadyExists,
	"PERMISSION_DENIED": codes.PermissionDenied, "UNAUTHENTICATED": codes.Unauthenticated, "UNAVAILABLE": codes.Unavailable,
	"DEADLINE_EXCEEDED": codes.DeadlineExceeded, "RESOURCE_EXHAUSTED": codes.ResourceExhausted, "ABORTED": codes.Aborted,
	"UNIMPLEMENTED": codes.Unimplemented, "INTERNAL": codes.Internal, "FAILED_PRECONDITION": codes.FailedPrecondition,
	"CANCELLED": codes.Canceled, "OUT_OF_RANGE": codes.OutOfRange, "DATA_LOSS": codes.DataLoss,
}

func httpStatusError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var body struct {
		Error errorBody `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Code != "" {
		return body.Error.status()
	}
	code := codes.Unknown
	switch resp.StatusCode {
	case http.StatusBadRequest:
		code = codes.InvalidArgument
	case http.StatusUnauthorized:
		code = codes.Unauthenticated
	case http.StatusForbidden:
		code = codes.PermissionDenied
	case http.StatusNotFound:
		code = codes.NotFound
	case http.StatusTooManyRequests:
		code = codes.ResourceExhausted
	case http.StatusNotImplemented:
		code = codes.Unimplemented
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		code = codes.Unavailable
	case http.StatusGatewayTimeout:
		code = codes.DeadlineExceeded
	}
	return status.Error(code, fmt.Sprintf("plugin http status %d", resp.StatusCode))
}
