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
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRpc2Body(t *testing.T) {
	var b bodyAndTrailer
	b.remain = -1
	require.Panics(t, func() { b.tryReadTrailer() })

	b.remain = 0
	_, err := b.WriteTo(io.Discard)
	require.ErrorIs(t, io.EOF, err)

	b.remain = 1
	_, err = b.WriteTo(io.Discard)
	require.ErrorIs(t, ErrLimitedWriter, err)
	_, err = b.WriteTo(LimitWriter(io.Discard, 2))
	require.ErrorIs(t, io.ErrShortWrite, err)
}

func TestRpc2ReadFrame(t *testing.T) {
	addr, cli, shutdown := newTcpServer()
	defer shutdown()

	cli.connector = defaultConnector(cli.ConnectorConfig)
	{
		conn, err := cli.connector.Get(testCtx, addr)
		require.NoError(t, err)
		frame, _ := conn.AllocFrame(1)
		frame.Write([]byte{0xee})
		conn.WriteFrame(frame)
		_, err = conn.ReadFrame()
		require.ErrorIs(t, io.EOF, err)
	}
	{
		conn, err := cli.connector.Get(testCtx, addr)
		require.NoError(t, err)
		frame, _ := conn.AllocFrame(5)
		frame.Write([]byte{0x1, 0x00, 0x00, 0x00})
		conn.WriteFrame(frame)
		_, err = conn.ReadFrame()
		require.ErrorIs(t, io.EOF, err)
	}
	{
		conn, err := cli.connector.Get(testCtx, addr)
		require.NoError(t, err)
		frame, _ := conn.AllocFrame(5)
		frame.Write([]byte{0x1, 0x00, 0x00, 0x00, 0xee})
		conn.WriteFrame(frame)
		_, err = conn.ReadFrame()
		require.ErrorIs(t, io.EOF, err)
	}
}