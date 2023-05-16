package blobnode

import (
	"context"
	"sync"
	"time"

	"github.com/cubefs/cubefs/blobstore/blobnode/core"
	"github.com/cubefs/cubefs/blobstore/common/trace"
)

type LimitRequestConf struct {
	Concurrency    int   `json:"concurrency"` // goroutine count
	QueueCnt       int64 `json:"queue_cnt"`   // queue element count
	TimeoutS       int64 `json:"timeout_s"`
	WaitIntervalMs int64 `json:"wait_interval_ms"`
}

type LimitRequestMgr struct {
	conf    LimitRequestConf
	queue   chan *core.Shard
	allReqs *limitReqQ
}

func NewLimitRequestMgr(conf LimitRequestConf) *LimitRequestMgr {
	return &LimitRequestMgr{
		conf:    conf,
		queue:   make(chan *core.Shard, conf.QueueCnt),
		allReqs: NewLimitReqQ(conf.QueueCnt),
	}
}

func (m *LimitRequestMgr) enqueue(cs core.ChunkAPI, shard *core.Shard) {
	m.queue <- shard
	m.allReqs.Add(cs, shard)
}

func (m *LimitRequestMgr) dequeue(shard *core.Shard) {
	//<-m.queue // enqueue:1,2,3,4 ; but 2 is first done?
	m.allReqs.Del(shard)
}

func (m *LimitRequestMgr) write(ctx1 context.Context, cs core.ChunkAPI, shard *core.Shard) error {
	ctx, cancel := context.WithTimeout(ctx1, time.Duration(m.conf.TimeoutS)*time.Second)
	defer cancel()
	span := trace.SpanFromContextSafe(ctx)
	m.enqueue(cs, shard)

	tk := time.NewTicker(time.Millisecond * time.Duration(m.conf.WaitIntervalMs))
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			m.allReqs.Del(shard)
			return ctx.Err()
		case <-tk.C:
			if m.allReqs.CheckDone(shard) {
				break
			}
		}
		break
	}

	err := m.allReqs.GetError(shard)
	m.dequeue(shard)
	span.Debugf("err:%v, cs:%v, shard:%v, qSize:%d, qCnt:%d", err, cs, shard, m.allReqs.Size(), m.allReqs.Count())
	return err
}

func (m *LimitRequestMgr) Run() {
	ctx := context.Background()

	for i := 0; i < m.conf.Concurrency; i++ {
		go func() {
			// get data
			shard := <-m.queue
			val, ok := m.allReqs.GetReq(shard)
			if ok {
				cs := val.cs
				err := cs.Write(ctx, shard)
				m.allReqs.SetDone(shard, err)
			}
			// !ok: it already delete
		}()
	}
}

type limitReqData struct {
	shard *core.Shard
	cs    core.ChunkAPI
	done  bool
	err   error
}

type limitReqQ struct {
	rmlck  sync.RWMutex
	datas  map[string]*limitReqData
	size   int64 // data payload size
	maxCnt int64
	count  int64
}

func NewLimitReqQ(max int64) *limitReqQ {
	ret := limitReqQ{
		datas:  make(map[string]*limitReqData),
		maxCnt: max,
	}
	return &ret
}

func (q *limitReqQ) Add(cs core.ChunkAPI, shard *core.Shard) bool {
	if q.Exist(shard.KeyString()) { // It already exists
		return false
	}

	q.rmlck.Lock()
	defer q.rmlck.Unlock()
	if q.count+1 > q.maxCnt {
		return false
	}

	q.datas[shard.KeyString()] = &limitReqData{shard: shard, cs: cs}
	q.size += int64(shard.Size)
	q.count++
	return true
}

func (q *limitReqQ) Del(req *core.Shard) bool {
	if !q.Exist(req.KeyString()) { // not exists
		return false
	}

	q.rmlck.Lock()
	defer q.rmlck.Unlock()
	delete(q.datas, req.KeyString())
	q.size -= int64(req.Size)
	q.count--
	return true
}

func (q *limitReqQ) Exist(key string) bool {
	ok := false
	q.rmlck.RLock()
	_, ok = q.datas[key]
	q.rmlck.RUnlock()
	return ok
}

func (q *limitReqQ) Size() int64 {
	q.rmlck.RLock()
	defer q.rmlck.RUnlock()
	return q.size
}

func (q *limitReqQ) Count() int64 {
	q.rmlck.RLock()
	defer q.rmlck.RUnlock()
	return q.count
}

func (q *limitReqQ) Clear() {
	q.rmlck.Lock()
	defer q.rmlck.Unlock()
	q.size = 0
	q.maxCnt = 0
	q.count = 0
	q.datas = make(map[string]*limitReqData)
}

func (q *limitReqQ) CheckDone(req *core.Shard) bool {
	q.rmlck.RLock()
	defer q.rmlck.RUnlock()

	ret := false
	if val, ok := q.datas[req.KeyString()]; ok {
		ret = val.done
	}
	return ret
}

func (q *limitReqQ) GetError(req *core.Shard) error {
	data, ok := q.GetReq(req)
	if ok {
		return data.err
	}
	return nil
}

func (q *limitReqQ) GetReq(req *core.Shard) (limitReqData, bool) {
	q.rmlck.RLock()
	defer q.rmlck.RUnlock()

	ret, ok := q.datas[req.KeyString()]
	if ok {
		return *ret, ok
	}
	return limitReqData{}, false
}

func (q *limitReqQ) SetDone(req *core.Shard, err error) {
	q.rmlck.Lock()
	defer q.rmlck.Unlock()
	data := q.datas[req.KeyString()]
	data.done = true
	data.err = err
}
