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
	"io"
)

func (fh *FixedHeader) ToHeader() Header {
	h := Header{
		M: make(map[string]string, len(fh.M)),
	}
	for k, v := range fh.M {
		h.M[k] = v.GetValue()
	}
	return h
}

func (fh *FixedHeader) AllSize() int {
	return 0
}

func (fh *FixedHeader) Reader() io.Reader {
	return bytes.NewReader(make([]byte, 0))
}

func (fh *FixedHeader) ReadFrom(r io.Reader) (int64, error) {
	return 0, nil
}

// func (fh FixedHeader) Set(key, value string)
// func (fh FixedHeader) SetSize(key string, size int)