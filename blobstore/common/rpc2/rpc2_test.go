// Copyright 2024 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package rpc2

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cubefs/cubefs/blobstore/common/rpc2/transport"
	_ "github.com/cubefs/cubefs/blobstore/testing/nolog"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/blobstore/util/retry"
	proto "github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"
)

var (
	defHandler = &Router{}
	testCtx    = context.Background()
)

func init() {
	defHandler.Middleware(handleMiddleware1, handleMiddleware2)
	defHandler.Register("/", handleNone)
	defHandler.Register("/stream", handleStream)
	defHandler.Register("/error", handleError)
}

func handleMiddleware1(w ResponseWriter, req *Request) error {
	log.Info("handle middleware-1")
	return nil
}

func handleMiddleware2(w ResponseWriter, req *Request) error {
	log.Info("handle middleware-2")
	return nil
}

type noCopyReadWriter struct{}

func (noCopyReadWriter) Read(p []byte) (int, error)  { return len(p), nil }
func (noCopyReadWriter) Write(p []byte) (int, error) { return len(p), nil }

func handleNone(w ResponseWriter, req *Request) error {
	if req.ContentLength == 0 {
		req.Body.Close()
		return w.WriteOK(nil)
	}
	req.Body.WriteTo(LimitWriter(noCopyReadWriter{}, req.ContentLength))
	w.SetContentLength(req.ContentLength)
	w.WriteHeader(200, nil)
	_, err := w.ReadFrom(noCopyReadWriter{})
	return err
}

func handleError(w ResponseWriter, req *Request) error {
	return w.WriteHeader(500, nil)
}

func handleStream(_ ResponseWriter, req *Request) error {
	stream := GenericServerStream[noneCodec, noneCodec]{ServerStream: req.ServerStream()}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err = stream.Send(&noneCodec{}); err != nil {
			return err
		}
	}
}

func newTcpServer() (string, *Client, func()) {
	server, client, f := newServer("tcp", defHandler)
	return server.Name, client, f
}

func newServer(network string, handler Handler) (*Server, *Client, func()) {
	addr := getAddress(network)
	trans := transport.DefaultConfig()
	trans.Version = 2
	server := Server{
		Name:         addr,
		Addresses:    []NetworkAddress{{Network: network, Address: addr}},
		Transport:    trans,
		Handler:      handler,
		StatDuration: 777 * time.Millisecond,
	}
	server.RegisterOnShutdown(func() { log.Info("shutdown") })
	go func() {
		if err := server.Serve(); err != nil && err != ErrServerClosed {
			panic(err)
		}
	}()
	server.waitServe()
	client := Client{
		ConnectorConfig: ConnectorConfig{
			Transport:   trans,
			Network:     network,
			DialTimeout: 200 * time.Millisecond,
		},
		RetryOn: func(err error) bool {
			status := DetectStatusCode(err)
			return status >= 500
		},
	}
	return &server, &client, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		server.Shutdown(ctx)
		client.Close()
	}
}

func getAddress(network string) (addr string) {
	if err := retry.Timed(10, 1).On(func() error {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return err
		}
		if err = ln.Close(); err != nil {
			return err
		}
		addr = ln.Addr().String()
		return nil
	}); err != nil {
		panic(err)
	}
	return
}

func BenchmarkUploadDownload(b *testing.B) {
	handler := &Router{}
	handler.Register("/", handleNone)
	server, cli, shutdown := newServer("tcp", handler)
	defer shutdown()
	cli.ConnectorConfig.BufioReaderSize = 4 << 20
	// cli.ConnectorConfig.BufioWriterSize = 4 << 20
	cli.ConnectorConfig.BufioFlushDuration = 10 * time.Millisecond

	l := int64(1 << 20)
	b.SetBytes(l)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		req, _ := NewRequest(testCtx, server.Name, "/", nil, noCopyReadWriter{})
		req.ContentLength = l
		resp, _ := cli.Do(req, nil)
		resp.Body.WriteTo(LimitWriter(noCopyReadWriter{}, l))
		resp.Body.Close()
	}
}

func TestRpc2None(t *testing.T) {
	{
		var x noneCodec
		x.Size()
		x.Marshal()
		x.MarshalTo(nil)
		x.Unmarshal(nil)
	}
	{
		NoBody.Read(nil)
		NoBody.WriteTo(nil)
		NoBody.Close()
	}
	{
		NoParameter.Size()
		NoParameter.Marshal()
		NoParameter.MarshalTo(nil)
		NoParameter.Unmarshal(nil)
	}
	{
		require.Panics(t, func() {
			var x codecReadWriter
			x.Size()
		})
	}
}

func TestRpc2Pb(t *testing.T) {
	{
		StreamCmd_NOT.EnumDescriptor()
		ChecksumAlgorithm_Alg_None.EnumDescriptor()
		ChecksumDirection_Dir_None.EnumDescriptor()
	}

	run := func(m interface {
		Codec
		Reset()
		String() string
		ProtoMessage()
		Descriptor() ([]byte, []int)
		XXX_Unmarshal(b []byte) error
		XXX_Marshal(b []byte, deterministic bool) ([]byte, error)
		XXX_Merge(src proto.Message)
		XXX_Size() int
		XXX_DiscardUnknown()
	}, discard bool,
	) {
		_ = m.String()
		m.ProtoMessage()
		m.Descriptor()
		b, _ := m.Marshal()
		m.XXX_Unmarshal(b)
		m.XXX_Marshal(b, false)
		m.XXX_Marshal(b, true)
		m.XXX_Merge(m)
		m.XXX_Size()
		if !discard {
			m.XXX_DiscardUnknown()
		}
		m.MarshalTo(b)
		m.Reset()
	}

	{
		var v *FixedValue
		v.GetLen()
		v.GetValue()
		v = &FixedValue{Len: 1, Value: "a"}
		run(v, false)
		v.GetLen()
		v.GetValue()
	}
	{
		var v *Header
		v.GetM()
		v.Getstable()
		v = &Header{}
		v.Set("a", "a")
		run(v, false)
		v.GetM()
		v.Getstable()
	}
	{
		var v *FixedHeader
		v.GetM()
		v.Getstable()
		v = &FixedHeader{}
		v.Set("a", "a")
		run(v, false)
		v.GetM()
		v.Getstable()
	}
	{
		var v *RequestHeader
		v.GetVersion()
		v.GetMagic()
		v.GetStreamCmd()
		v.GetRemotePath()
		v.GetTraceID()
		v.GetContentLength()
		v.GetHeader()
		v.GetTrailer()
		v.GetParameter()
		v.XXX_DiscardUnknown()
		v = &RequestHeader{
			Version:       1,
			Magic:         1,
			StreamCmd:     StreamCmd_NOT,
			RemotePath:    "/",
			TraceID:       "xxx",
			ContentLength: 10,
			Header:        Header{},
			Trailer:       FixedHeader{},
			Parameter:     []byte{0xff},
		}
		v.Header.Set("a", "a")
		v.Trailer.Set("b", "b")
		run(v, true)
		v.GetVersion()
		v.GetMagic()
		v.GetStreamCmd()
		v.GetRemotePath()
		v.GetTraceID()
		v.GetContentLength()
		v.GetHeader()
		v.GetTrailer()
		v.GetParameter()
	}
	{
		var v *ResponseHeader
		v.GetVersion()
		v.GetMagic()
		v.GetStatus()
		v.GetReason()
		v.GetError()
		v.GetContentLength()
		v.GetHeader()
		v.GetTrailer()
		v.GetParameter()
		v.XXX_DiscardUnknown()
		v = &ResponseHeader{
			Version:       1,
			Magic:         1,
			Status:        200,
			Reason:        "R",
			Error:         "E",
			ContentLength: 10,
			Header:        Header{},
			Trailer:       FixedHeader{},
			Parameter:     []byte{0xff},
		}
		v.Header.Set("a", "a")
		v.Trailer.Set("b", "b")
		run(v, true)
		v.GetVersion()
		v.GetMagic()
		v.GetStatus()
		v.GetReason()
		v.GetError()
		v.GetContentLength()
		v.GetHeader()
		v.GetTrailer()
		v.GetParameter()
	}
	{
		var v *Error
		v.GetStatus()
		v.GetReason()
		v.GetDetail()
		v = &Error{
			Status: 100,
			Reason: "R",
			Detail: "E",
		}
		run(v, false)
		v.GetStatus()
		v.GetReason()
		v.GetDetail()
	}
	{
		var v *ChecksumBlock
		v.GetAlgorithm()
		v.GetDirection()
		v.GetBlockSize()
		v = &ChecksumBlock{
			Algorithm: ChecksumAlgorithm_Crc_IEEE,
			Direction: ChecksumDirection_Duplex,
			BlockSize: 1 << 10,
		}
		run(v, false)
		v.GetAlgorithm()
		v.GetDirection()
		v.GetBlockSize()
	}
}