// Code generated by protoc-gen-gogo. DO NOT EDIT.
// source: service.proto

package raft

import (
	context "context"
	fmt "fmt"
	proto "github.com/gogo/protobuf/proto"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	math "math"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion3 // please upgrade the proto package

func init() { proto.RegisterFile("service.proto", fileDescriptor_a0b84a42fa06f626) }

var fileDescriptor_a0b84a42fa06f626 = []byte{
	// 181 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xe2, 0xe2, 0x2d, 0x4e, 0x2d, 0x2a,
	0xcb, 0x4c, 0x4e, 0xd5, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0x92, 0x49, 0x2e, 0x4d, 0x4a, 0x4d,
	0x2b, 0xd6, 0x4b, 0xca, 0xc9, 0x4f, 0x2a, 0x2e, 0xc9, 0x2f, 0x4a, 0xd5, 0x4b, 0xce, 0xcf, 0xcd,
	0xcd, 0xcf, 0xd3, 0x2b, 0x4a, 0x4c, 0x2b, 0x91, 0xe2, 0x02, 0x91, 0x10, 0x95, 0x46, 0xfd, 0x4c,
	0x5c, 0xdc, 0x41, 0x89, 0x69, 0x25, 0xc1, 0x10, 0xfd, 0x42, 0x4d, 0x8c, 0x5c, 0x02, 0x20, 0xbe,
	0x6f, 0x6a, 0x71, 0x71, 0x62, 0x7a, 0xaa, 0x53, 0x62, 0x49, 0x72, 0x86, 0x90, 0xa9, 0x1e, 0x3e,
	0xf3, 0xf4, 0x90, 0xd4, 0x07, 0xa5, 0x16, 0x96, 0xa6, 0x16, 0x97, 0x80, 0xb5, 0x49, 0x19, 0x92,
	0xa0, 0xad, 0xb8, 0x20, 0x3f, 0xaf, 0x38, 0x55, 0x89, 0x41, 0x83, 0xd1, 0x80, 0x51, 0xa8, 0x9a,
	0x8b, 0x07, 0xec, 0xa6, 0xbc, 0xc4, 0x82, 0xe2, 0x8c, 0xfc, 0x12, 0x21, 0x22, 0x0c, 0x82, 0xa9,
	0x85, 0x3a, 0x40, 0xca, 0x88, 0x14, 0x2d, 0xc8, 0x96, 0x3b, 0x71, 0x46, 0xb1, 0xeb, 0xe9, 0x5b,
	0x83, 0x54, 0x25, 0xb1, 0x81, 0xc3, 0xc8, 0x18, 0x10, 0x00, 0x00, 0xff, 0xff, 0x7e, 0xdb, 0x7f,
	0x5e, 0x5e, 0x01, 0x00, 0x00,
}

// Reference imports to suppress errors if they are not otherwise used.
var _ context.Context
var _ grpc.ClientConn

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
const _ = grpc.SupportPackageIsVersion4

// RaftServiceClient is the client API for RaftService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://godoc.org/google.golang.org/grpc#ClientConn.NewStream.
type RaftServiceClient interface {
	RaftMessageBatch(ctx context.Context, opts ...grpc.CallOption) (RaftService_RaftMessageBatchClient, error)
	RaftSnapshot(ctx context.Context, opts ...grpc.CallOption) (RaftService_RaftSnapshotClient, error)
}

type raftServiceClient struct {
	cc *grpc.ClientConn
}

func NewRaftServiceClient(cc *grpc.ClientConn) RaftServiceClient {
	return &raftServiceClient{cc}
}

func (c *raftServiceClient) RaftMessageBatch(ctx context.Context, opts ...grpc.CallOption) (RaftService_RaftMessageBatchClient, error) {
	stream, err := c.cc.NewStream(ctx, &_RaftService_serviceDesc.Streams[0], "/cubefs.blobstore.common.raft.RaftService/RaftMessageBatch", opts...)
	if err != nil {
		return nil, err
	}
	x := &raftServiceRaftMessageBatchClient{stream}
	return x, nil
}

type RaftService_RaftMessageBatchClient interface {
	Send(*RaftMessageRequestBatch) error
	Recv() (*RaftMessageResponse, error)
	grpc.ClientStream
}

type raftServiceRaftMessageBatchClient struct {
	grpc.ClientStream
}

func (x *raftServiceRaftMessageBatchClient) Send(m *RaftMessageRequestBatch) error {
	return x.ClientStream.SendMsg(m)
}

func (x *raftServiceRaftMessageBatchClient) Recv() (*RaftMessageResponse, error) {
	m := new(RaftMessageResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *raftServiceClient) RaftSnapshot(ctx context.Context, opts ...grpc.CallOption) (RaftService_RaftSnapshotClient, error) {
	stream, err := c.cc.NewStream(ctx, &_RaftService_serviceDesc.Streams[1], "/cubefs.blobstore.common.raft.RaftService/RaftSnapshot", opts...)
	if err != nil {
		return nil, err
	}
	x := &raftServiceRaftSnapshotClient{stream}
	return x, nil
}

type RaftService_RaftSnapshotClient interface {
	Send(*RaftSnapshotRequest) error
	Recv() (*RaftSnapshotResponse, error)
	grpc.ClientStream
}

type raftServiceRaftSnapshotClient struct {
	grpc.ClientStream
}

func (x *raftServiceRaftSnapshotClient) Send(m *RaftSnapshotRequest) error {
	return x.ClientStream.SendMsg(m)
}

func (x *raftServiceRaftSnapshotClient) Recv() (*RaftSnapshotResponse, error) {
	m := new(RaftSnapshotResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// RaftServiceServer is the server API for RaftService service.
type RaftServiceServer interface {
	RaftMessageBatch(RaftService_RaftMessageBatchServer) error
	RaftSnapshot(RaftService_RaftSnapshotServer) error
}

// UnimplementedRaftServiceServer can be embedded to have forward compatible implementations.
type UnimplementedRaftServiceServer struct {
}

func (*UnimplementedRaftServiceServer) RaftMessageBatch(srv RaftService_RaftMessageBatchServer) error {
	return status.Errorf(codes.Unimplemented, "method RaftMessageBatch not implemented")
}
func (*UnimplementedRaftServiceServer) RaftSnapshot(srv RaftService_RaftSnapshotServer) error {
	return status.Errorf(codes.Unimplemented, "method RaftSnapshot not implemented")
}

func RegisterRaftServiceServer(s *grpc.Server, srv RaftServiceServer) {
	s.RegisterService(&_RaftService_serviceDesc, srv)
}

func _RaftService_RaftMessageBatch_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(RaftServiceServer).RaftMessageBatch(&raftServiceRaftMessageBatchServer{stream})
}

type RaftService_RaftMessageBatchServer interface {
	Send(*RaftMessageResponse) error
	Recv() (*RaftMessageRequestBatch, error)
	grpc.ServerStream
}

type raftServiceRaftMessageBatchServer struct {
	grpc.ServerStream
}

func (x *raftServiceRaftMessageBatchServer) Send(m *RaftMessageResponse) error {
	return x.ServerStream.SendMsg(m)
}

func (x *raftServiceRaftMessageBatchServer) Recv() (*RaftMessageRequestBatch, error) {
	m := new(RaftMessageRequestBatch)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func _RaftService_RaftSnapshot_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(RaftServiceServer).RaftSnapshot(&raftServiceRaftSnapshotServer{stream})
}

type RaftService_RaftSnapshotServer interface {
	Send(*RaftSnapshotResponse) error
	Recv() (*RaftSnapshotRequest, error)
	grpc.ServerStream
}

type raftServiceRaftSnapshotServer struct {
	grpc.ServerStream
}

func (x *raftServiceRaftSnapshotServer) Send(m *RaftSnapshotResponse) error {
	return x.ServerStream.SendMsg(m)
}

func (x *raftServiceRaftSnapshotServer) Recv() (*RaftSnapshotRequest, error) {
	m := new(RaftSnapshotRequest)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

var _RaftService_serviceDesc = grpc.ServiceDesc{
	ServiceName: "cubefs.blobstore.common.raft.RaftService",
	HandlerType: (*RaftServiceServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "RaftMessageBatch",
			Handler:       _RaftService_RaftMessageBatch_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
		{
			StreamName:    "RaftSnapshot",
			Handler:       _RaftService_RaftSnapshot_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "service.proto",
}