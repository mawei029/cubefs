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
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/cubefs/cubefs/blobstore/common/rpc2/transport"
	"github.com/cubefs/cubefs/blobstore/util/retry"
)

type Client struct {
	Connector interface {
		Get(ctx context.Context, addr string) (*transport.Stream, error)
		Put(ctx context.Context, stream *transport.Stream) error
	}

	Retry int
	// | Request | Response Header |   Response Body  |
	// |      Request Timeout      | Response Timeout |
	// |                 Timeout                      |
	Timeout         time.Duration
	RequestTimeout  time.Duration
	ResponseTimeout time.Duration
}

func (c *Client) Do(req *Request) (resp *Response, err error) {
	try := c.Retry
	if try <= 0 {
		try = 3
	}
	err = retry.Timed(try, 1).RuptOn(func() (bool, error) {
		resp, err = c.do(req)
		if err != nil {
			if req.Body == nil {
				return true, err
			}
			body, errBody := req.GetBody()
			if errBody != nil {
				return true, err
			}
			req.Body = clientNopBody(body)
			return false, err
		}
		return true, err
	})
	return
}

func (c *Client) do(req *Request) (*Response, error) {
	conn, err := c.Connector.Get(req.Context(), req.RemoteAddr)
	if err != nil {
		return nil, err
	}
	req.cli = c
	req.conn = conn

	resp, err := req.request(c.requestDeadline(req.Context()))
	if err != nil {
		req.conn.Close()
		return nil, err
	}
	req.conn.SetReadDeadline(c.responseDeadline(req.Context()))
	return resp, nil
}

func (c *Client) requestDeadline(ctx context.Context) time.Time {
	var timeout, reqTimeout time.Time
	if c.Timeout > 0 {
		timeout = time.Now().Add(c.Timeout)
	}
	if c.RequestTimeout > 0 {
		reqTimeout = time.Now().Add(c.RequestTimeout)
	}
	return beforeContextDeadline(ctx, latestTime(timeout, reqTimeout))
}

func (c *Client) responseDeadline(ctx context.Context) time.Time {
	var timeout, respTimeout time.Time
	if c.Timeout > 0 {
		timeout = time.Now().Add(c.Timeout)
	}
	if c.ResponseTimeout > 0 {
		respTimeout = time.Now().Add(c.ResponseTimeout)
	}
	return beforeContextDeadline(ctx, latestTime(timeout, respTimeout))
}

func NewRequest(ctx context.Context, addr, handler string, body io.Reader) *Request {
	rc, ok := body.(io.ReadCloser)
	if !ok && body != nil {
		rc = io.NopCloser(body)
	}
	req := &Request{
		RequestHeader: RequestHeader{
			Version:       Version,
			Magic:         Magic,
			RemoteAddr:    addr,
			RemoteHandler: handler,
			TraceID:       getSpan(ctx).TraceID(),
		},
		ctx:       ctx,
		Body:      clientNopBody(rc),
		AfterBody: func() error { return nil },
	}
	if body != nil {
		switch v := body.(type) {
		case *bytes.Buffer:
			req.ContentLength = int64(v.Len())
			buf := v.Bytes()
			req.GetBody = func() (io.ReadCloser, error) {
				r := bytes.NewReader(buf)
				return io.NopCloser(r), nil
			}
		case *bytes.Reader:
			req.ContentLength = int64(v.Len())
			snapshot := *v
			req.GetBody = func() (io.ReadCloser, error) {
				r := snapshot
				return io.NopCloser(&r), nil
			}
		case *strings.Reader:
			req.ContentLength = int64(v.Len())
			snapshot := *v
			req.GetBody = func() (io.ReadCloser, error) {
				r := snapshot
				return io.NopCloser(&r), nil
			}
		default:
		}
		if req.GetBody != nil && req.ContentLength == 0 {
			req.Body = NoBody
			req.GetBody = func() (io.ReadCloser, error) { return NoBody, nil }
		}
	}
	return req
}

type StreamClient[Req any, Res any] struct {
	Client *Client
}

func (sc *StreamClient[Req, Res]) Streaming(req *Request) (StreamingClient[Req, Res], error) {
	resp, err := sc.Client.Do(req)
	if err != nil {
		return nil, err
	}
	cs := &clientStream{
		req:     req,
		header:  resp.Header,
		trailer: resp.Trailer.ToHeader(),
	}
	return &GenericClientStream[Req, Res]{ClientStream: cs}, nil
}

func NewStreamRequest(ctx context.Context, addr, handler string) *Request {
	return &Request{
		RequestHeader: RequestHeader{
			Version:       Version,
			Magic:         Magic,
			RemoteAddr:    addr,
			StreamCmd:     StreamCmd_SYN,
			RemoteHandler: handler,
			TraceID:       getSpan(ctx).TraceID(),
		},
		ctx:       ctx,
		Body:      NoBody,
		AfterBody: func() error { return nil },
	}
}