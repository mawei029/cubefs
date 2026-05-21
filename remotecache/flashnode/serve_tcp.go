// Copyright 2023 The CubeFS Authors.
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

package flashnode

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
)

// NewFlashNode creates a new flash node instance.
func NewFlashNode() *FlashNode {
	return &FlashNode{}
}

// StartTcpService binds and listens to the specified port.
func (f *FlashNode) startTcpServer() (err error) {
	f.tcpListener, err = net.Listen("tcp", ":"+f.listen)
	if err != nil {
		return
	}
	go func() {
		defer f.tcpListener.Close()
		var latestAlarm time.Time
		var latestConnectionLimitAlarm time.Time
		for {
			conn, err1 := f.tcpListener.Accept()

			select {
			case <-f.stopCh:
				log.LogWarn("remotecache tcp server stopped")
				return
			default:
			}

			if err1 != nil {
				log.LogErrorf("action[tcpAccept] failed err:%s", err1.Error())
				// Alarm only once at 1 minute
				if time.Since(latestAlarm) > time.Minute {
					warnMsg := fmt.Sprintf("SERVER ACCEPT CONNECTION FAILED!\n"+
						"Failed on accept connection from %v and will retry after 1s.\n"+
						"Error message: %s", f.tcpListener.Addr(), err1.Error())
					exporter.Warning(warnMsg)
					latestAlarm = time.Now()
				}
				time.Sleep(time.Second)
				continue
			}
			activeConnections := atomic.AddInt64(&f.activeConnections, 1)
			connectionLimit := atomic.LoadInt64(&f.connectionLimit)
			if connectionLimit > 0 && activeConnections > connectionLimit {
				atomic.AddInt64(&f.activeConnections, -1)
				p := proto.NewPacket()
				p.PacketErrorWithBody(proto.OpErr, []byte(proto.ErrFlashNodeConnectionLimited.Error()))
				if err := p.WriteToConn(conn); err != nil {
					log.LogWarnf("flashnode write connection limit error failed, active:%d limit:%d err:%v", activeConnections, connectionLimit, err)
				}
				conn.Close()
				if time.Since(latestConnectionLimitAlarm) > time.Minute {
					log.LogWarnf("flashnode active connection limit exceeded, active:%d limit:%d", activeConnections, connectionLimit)
					latestConnectionLimitAlarm = time.Now()
				}
				continue
			}
			go f.serveConn(conn)
		}
	}()
	log.LogInfo("started tcp server")
	return
}

func (f *FlashNode) stopServer() {
	f.tcpListener.Close()
}

func (f *FlashNode) serveConn(conn net.Conn) {
	defer func() {
		atomic.AddInt64(&f.activeConnections, -1)
		conn.Close()
	}()
	c := conn.(*net.TCPConn)
	c.SetKeepAlive(true)
	c.SetNoDelay(true)

	remoteAddr := conn.RemoteAddr().String()
	for {
		c.SetReadDeadline(time.Now().Add(time.Second * _tcpServerTimeoutSec))
		select {
		case <-f.stopCh:
			return
		default:
		}

		p := proto.NewPacketReqID()
		if err := p.ReadFromConn(c, proto.NoReadDeadlineTime); err != nil {
			if err != io.EOF {
				log.LogWarn("remotecache read from remote", err.Error())
			}
			return
		}
		if err := f.preHandle(conn, p); err != nil {
			log.LogWarn("preHandle", p.LogMessage(p.GetOpMsg(), remoteAddr, p.StartT, err))
			p.PacketErrorWithBody(proto.OpErr, ([]byte)(err.Error()))
			p.WriteToConn(conn)
			// Keep the connection open after readLimiter rejects a request. The
			// SDK treats flashnode limit errors as reusable-connection errors and
			// may put this TCP connection back into its pool. Closing here would
			// leave a server-closed connection in the client pool and cause the
			// next reuse to fail before reconnecting.
			continue
		}
		f.handlePacket(conn, p)
	}
}
