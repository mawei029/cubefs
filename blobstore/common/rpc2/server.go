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
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/blobstore/common/rpc2/transport"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

type Handler interface {
	Handle(ResponseWriter, *Request) error
}

type Server struct {
	Name string

	Handler Handler

	// |   Request Header  | Request Body | Response Header Body |
	// | ReadHeaderTimeout |
	// |          ReadTimeout             |     WriteTimeout     |
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration

	Transport *transport.Config

	StatDuration time.Duration
	statOnce     sync.Once

	inShutdown atomic.Bool // true when server is in shutdown

	listenerGroup sync.WaitGroup
	mu            sync.Mutex
	listeners     map[*net.Listener]struct{}
	sessions      map[*transport.Session]struct{}
	onShutdown    []func()
}

func (s *Server) stating() {
	s.statOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(s.StatDuration)
			defer ticker.Stop()
			for range ticker.C {
				if s.shuttingDown() {
					return
				}
				log.Debug("stating on", s.Name)
				s.mu.Lock()
				log.Debugf("server has %d listeners", len(s.listeners))
				log.Debugf("server has %d sessions", len(s.sessions))
				for sess := range s.sessions {
					log.Debugf("session %v has %d streams", sess.LocalAddr(), sess.NumStreams())
				}
				s.mu.Unlock()
			}
		}()
	})
}

func (s *Server) shuttingDown() bool {
	return s.inShutdown.Load()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.inShutdown.Store(true)

	var err error
	s.mu.Lock()
	for ln := range s.listeners {
		if cerr := (*ln).Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	for _, f := range s.onShutdown {
		go f()
	}
	s.mu.Unlock()

	log.Warn("shutdown and try to sleep 5 senconds")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	return err
}

func (s *Server) RegisterOnShutdown(f func()) {
	s.mu.Lock()
	s.onShutdown = append(s.onShutdown, f)
	s.mu.Unlock()
}

func (s *Server) Serve(ln net.Listener) error {
	err := s.Listen(ln)
	s.listenerGroup.Wait()
	return err
}

func (s *Server) Listen(ln net.Listener) error {
	ln = &onceCloseListener{Listener: ln}

	key := &ln
	s.mu.Lock()
	if s.listeners == nil {
		s.listeners = make(map[*net.Listener]struct{})
	}
	if s.sessions == nil {
		s.sessions = make(map[*transport.Session]struct{})
	}
	_, has := s.listeners[key]
	s.mu.Unlock()
	if has {
		return nil
	}

	s.mu.Lock()
	s.listeners[key] = struct{}{}
	s.listenerGroup.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, has := s.listeners[key]; has {
			ln.Close()
			delete(s.listeners, key)
			s.listenerGroup.Done()
		}
		s.mu.Unlock()
	}()

	if s.StatDuration > 0 {
		s.stating()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.shuttingDown() {
				return ErrServerClosed
			}
			log.Errorf("listener %v accept, %s", ln.Addr(), err.Error())
			return err
		}

		sess, err := transport.Server(conn, s.Transport)
		if err != nil {
			log.Errorf("listener %v transport %v, %s",
				ln.Addr(), conn.RemoteAddr(), err.Error())
			return err
		}
		go s.handleSession(sess)
	}
}

func (s *Server) handleSession(sess *transport.Session) {
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	for {
		if stream, err := sess.AcceptStream(); err == nil {
			go s.handleStream(stream)
		} else {
			log.Errorf("session %v accept stream %v, %s",
				sess.LocalAddr(), sess.RemoteAddr(), err.Error())
			break
		}
	}
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
}

func (s *Server) handleStream(stream *transport.Stream) {
	ctx := context.Background()
	if err := func() error {
		for {
			req, err := s.readRequest(stream)
			if err != nil {
				return err
			}
			ctx = req.Context()

			resp := &response{}
			if err = s.Handler.Handle(resp, req); err != nil {
				return err
			}

			respHeaderSize := resp.hdr.Size()
			if _headerCell+respHeaderSize > stream.MaxPayloadSize() {
				return ErrHeaderFrame
			}

			var cell headerCell
			cell.Set(respHeaderSize)
			size := _headerCell + respHeaderSize + int(resp.hdr.ContentLength) + resp.hdr.Trailer.AllSize()
			_, err = stream.SizedWrite(io.MultiReader(cell.Reader(),
				resp.hdr.MarshalToReader(),
				// io.LimitReader(req.Body, req.ContentLength),
				req.trailerReader(),
			), size)
			if err != nil {
				return err
			}
		}
	}(); err != nil {
		span := trace.SpanFromContextSafe(ctx)
		span.Errorf("stream(%d, %v, %v) %s", stream.ID(), stream.LocalAddr(), stream.RemoteAddr(), err.Error())
	}
}

func (s *Server) readRequest(stream *transport.Stream) (*Request, error) {
	var hdr RequestHeader
	frame, err := readHeaderFrame(stream, &hdr)
	if err != nil {
		return nil, err
	}

	traceID := hdr.TraceID
	if traceID == "" {
		traceID = trace.RandomID().String()
	}
	_, ctx := trace.StartSpanFromContextWithTraceID(context.Background(), "", traceID)

	req := &Request{RequestHeader: hdr, ctx: ctx}
	req.Body = makeBodyWithTrailer(
		stream.NewSizedReader(int(req.ContentLength)+req.Trailer.AllSize(), frame),
		req.ContentLength, nil, &req.Trailer)
	return req, nil
}

type onceCloseListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (oc *onceCloseListener) Close() error {
	oc.once.Do(func() { oc.err = oc.Listener.Close() })
	return oc.err
}