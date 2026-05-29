// Copyright 2026 The CubeFS Authors.
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

// splitLruCapacityByDiskSpace assigns total block-LRU capacity across disks in proportion to
// each disk's TotalSpace. The returned slice sums to total. When total >= len(spaces), each
// entry is at least 1. The last disk absorbs rounding remainder so the sum is exact.
func splitLruCapacityByDiskSpace(total int, spaces []int64) []int {
	n := len(spaces)
	if n == 0 {
		return nil
	}
	caps := make([]int, n)
	if total <= 0 {
		return caps
	}
	if n == 1 {
		caps[0] = total
		return caps
	}

	var allSpace int64
	for _, s := range spaces {
		allSpace += s
	}
	if allSpace <= 0 {
		base := total / n
		rem := total % n
		for i := range caps {
			caps[i] = base
			if i < rem {
				caps[i]++
			}
		}
		return caps
	}

	enforceMinOne := total >= n
	assigned := 0
	for i := 0; i < n-1; i++ {
		c := int(float64(spaces[i]) / float64(allSpace) * float64(total))
		if enforceMinOne && c < 1 {
			c = 1
		}
		caps[i] = c
		assigned += c
	}
	caps[n-1] = total - assigned
	if enforceMinOne && caps[n-1] < 1 {
		caps[n-1] = 1
		over := assigned + 1 - total
		for i := 0; i < n-1 && over > 0; i++ {
			if caps[i] <= 1 {
				continue
			}
			d := caps[i] - 1
			if d > over {
				d = over
			}
			caps[i] -= d
			over -= d
		}
		caps[n-1] = total
		for i := 0; i < n-1; i++ {
			caps[n-1] -= caps[i]
		}
		if caps[n-1] < 1 {
			caps[n-1] = 1
		}
	}
	return caps
}
