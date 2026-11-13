package host

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
)

func chunkType(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	field := func(name string, n int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), JsonName: proto.String(name), Number: proto.Int32(n), Type: typ.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("chunk.proto"), Package: proto.String("test.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Chunk"), Field: []*descriptorpb.FieldDescriptorProto{
			field("name", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING), field("data", 2, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Chunk")
}

func chunk(md protoreflect.MessageDescriptor, name string, data []byte) *dynamicpb.Message {
	m := dynamicpb.NewMessage(md)
	if name != "" {
		m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString(name))
	}
	if data != nil {
		m.Set(md.Fields().ByName("data"), protoreflect.ValueOfBytes(data))
	}
	return m
}

func header(t *testing.T, r *http.Request) string {
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(MessageHeader))
	if err != nil {
		t.Errorf("message header: %v", err)
	}
	return string(raw)
}

func TestHTTPRawUploadAndDownload(t *testing.T) {
	md := chunkType(t)
	blob := bytes.Repeat([]byte("0123456789"), 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/test.v1.Blob/Put":
			body, _ := io.ReadAll(r.Body)
			if header(t, r) != `{"name":"f"}` || string(body) != "hello world" || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("put header %q body %q", header(t, r), body)
			}
			_, _ = w.Write([]byte(`{"name":"stored"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/test.v1.Blob/Open":
			if header(t, r) != `{"name":"f"}` {
				t.Errorf("get header %q", header(t, r))
			}
			w.Header().Set(MessageHeader, base64.RawURLEncoding.EncodeToString([]byte(`{"name":"meta"}`)))
			_, _ = w.Write(blob)
		default:
			http.Error(w, `{"error":{"code":"UNIMPLEMENTED","message":"no"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	conn := newHTTPConn(srv.URL, srv.Client(), "", 1<<20)

	up, err := conn.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true}, "/test.v1.Blob/Put")
	if err != nil {
		t.Fatal(err)
	}
	if err := up.SendMsg(chunk(md, "f", []byte("hello "))); err != nil {
		t.Fatal(err)
	}
	if err := up.SendMsg(chunk(md, "", []byte("world"))); err != nil {
		t.Fatal(err)
	}
	if err := up.CloseSend(); err != nil {
		t.Fatal(err)
	}
	reply := chunk(md, "", nil)
	if err := up.RecvMsg(reply); err != nil || reply.Get(md.Fields().ByName("name")).String() != "stored" {
		t.Fatalf("upload reply %v %v", reply, err)
	}

	down, err := conn.NewStream(t.Context(), &grpc.StreamDesc{ServerStreams: true}, "/test.v1.Blob/Open")
	if err != nil {
		t.Fatal(err)
	}
	if err := down.SendMsg(chunk(md, "f", nil)); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; ; i++ {
		m := chunk(md, "", nil)
		err := down.RecvMsg(m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if name := m.Get(md.Fields().ByName("name")).String(); (i == 0) != (name == "meta") {
			t.Fatalf("message %d name %q: metadata belongs to the first message only", i, name)
		}
		got = append(got, m.Get(md.Fields().ByName("data")).Bytes()...)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(blob))
	}
}

func TestHTTPRawUploadRules(t *testing.T) {
	md := chunkType(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	conn := newHTTPConn(srv.URL, srv.Client(), "", 1<<20)
	if _, err := conn.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, "/x/Y"); status.Code(err) != codes.Unimplemented {
		t.Fatalf("bidi = %v", err)
	}
	up, _ := conn.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true}, "/x/Y")
	if err := up.SendMsg(&acplugv1.HealthRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("message without a bytes field = %v", err)
	}
	up, _ = conn.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true}, "/x/Y")
	_ = up.SendMsg(chunk(md, "f", []byte("a")))
	if err := up.SendMsg(chunk(md, "again", []byte("b"))); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("metadata after the first message = %v", err)
	}
}
