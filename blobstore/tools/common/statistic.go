package statistic

import (
	"fmt"
	"sync/atomic"
	"time"
)

func NewTimeStatistic(module string, unitUs time.Duration, intervalNum int) *TimeStatistic {
	return &TimeStatistic{
		module:      module,
		buckets:     make([]int64, intervalNum+1),
		unitUs:      unitUs,
		intervalNum: intervalNum,
	}
}

type TimeStatistic struct {
	module        string
	intervalNum   int
	buckets       []int64
	unitUs        time.Duration
	totalReqNum   int64
	totalTimeCost int64
}

func (t *TimeStatistic) Set(costUs time.Duration) {
	atomic.AddInt64(&t.totalTimeCost, int64(costUs))
	atomic.AddInt64(&t.totalReqNum, 1)
	if int(costUs/t.unitUs) > t.intervalNum {
		atomic.AddInt64(&t.buckets[t.intervalNum], 1)
		return
	}
	atomic.AddInt64(&t.buckets[int(costUs/t.unitUs)], 1)
}

func (t *TimeStatistic) Report() {
	fmt.Printf("%s time cost[avg]: %fus\n", t.module, float64(t.totalTimeCost)/float64(t.totalReqNum)/float64(time.Microsecond))
	fmt.Printf("%s total request: %d, QPS: %f\n", t.module, t.totalReqNum, float64(time.Second)/(float64(t.totalTimeCost)/float64(t.totalReqNum)))

	p99ReqStartOffset := t.totalReqNum * 99 / 100
	p999ReqStartOffset := t.totalReqNum * 999 / 1000

	for metric, offset := range map[string]int64{"p99": p99ReqStartOffset, "p999": p999ReqStartOffset} {
		for i, num := range t.buckets {
			offset -= num
			if offset > 0 {
				continue
			}
			if i == t.intervalNum {
				fmt.Printf("%s time cost[%s] execeed the max interval bucket, you should change the unit us or interval num\n", t.module, metric)
				return
			}
			startOffset := num + offset
			fmt.Printf("%s time cost[%s]: %fus\n", t.module, metric, float64(time.Duration(i)*t.unitUs)/float64(time.Microsecond)+float64(t.unitUs)/float64(num)/float64(time.Microsecond)*float64(startOffset))
			break
		}
	}
}
