package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+method, bytes.NewReader(body))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
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
	if !desc.ServerStreams || desc.ClientStreams {
		return nil, status.Error(codes.Unimplemented, "http mapping supports unary and server-streaming methods only")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &httpStream{ctx: ctx, cancel: cancel, conn: h, method: method}, nil
}

type httpStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *httpConn
	method string

	mu   sync.Mutex
	sent bool
	body io.Closer
	scan *bufio.Reader
}

func (s *httpStream) Header() (metadata.MD, error) { return nil, nil }
func (s *httpStream) Trailer() metadata.MD         { return nil }
func (s *httpStream) Context() context.Context     { return s.ctx }
func (s *httpStream) CloseSend() error             { return nil }

func (s *httpStream) SendMsg(m any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent {
		return status.Error(codes.Internal, "server-streaming call sends one request")
	}
	s.sent = true
	resp, err := s.conn.request(s.ctx, s.method, m)
	if err != nil {
		return err
	}
	s.body = resp.Body
	s.scan = bufio.NewReaderSize(resp.Body, 64<<10)
	return nil
}

func (s *httpStream) RecvMsg(m any) error {
	s.mu.Lock()
	scan := s.scan
	s.mu.Unlock()
	if scan == nil {
		return status.Error(codes.Internal, "RecvMsg before SendMsg")
	}
	for {
		line, err := scan.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			return decodeFrame(line, m)
		}
		if err != nil {
			s.cancel()
			_ = s.body.Close()
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			if s.ctx.Err() != nil {
				return status.FromContextError(s.ctx.Err()).Err()
			}
			return status.Error(codes.Unavailable, "plugin stream broken")
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
