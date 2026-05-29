package flashgroupmanager

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashnode"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

type RaftCmd struct {
	Op uint32 `json:"op"`
	K  string `json:"k"`
	V  []byte `json:"v"`
}

type moveKeyValueCmd struct {
	NewK string `json:"new_k"`
	NewV []byte `json:"new_v"`
}

func (m *RaftCmd) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// Unmarshal converts the byte array to a RaftCmd.
func (m *RaftCmd) Unmarshal(data []byte) (err error) {
	return json.Unmarshal(data, m)
}

type clusterValue struct {
	Name                         string
	FlashNodeHandleReadTimeout   int
	FlashNodeReadDataNodeTimeout int
	RemoteCacheTTL               int64
	RemoteCacheReadTimeout       int64
	RemoteCacheMultiRead         bool
	FlashNodeTimeoutCount        int64
	RemoteCacheSameZoneTimeout   int64
	RemoteCacheSameRegionTimeout int64
	FlashHotKeyMissCount         int
	FlashNodeReadRps             int64
	MaxDisableFlashGroupPercent  int
	FlashReadFlowLimit           int64
	FlashWriteFlowLimit          int64
	FlashKeyFlowLimit            int64
	FlashNodeConnectionLimit     int64
	RemoteClientFlowLimit        int64
}

func newClusterValue(c *Cluster) (cv *clusterValue) {
	cv = &clusterValue{
		Name:                         c.Name,
		FlashNodeHandleReadTimeout:   c.cfg.FlashNodeHandleReadTimeout,
		FlashNodeReadDataNodeTimeout: c.cfg.FlashNodeReadDataNodeTimeout,
		RemoteCacheTTL:               c.cfg.RemoteCacheTTL,
		RemoteCacheReadTimeout:       c.cfg.RemoteCacheReadTimeout,
		RemoteCacheMultiRead:         c.cfg.RemoteCacheMultiRead,
		FlashNodeTimeoutCount:        c.cfg.FlashNodeTimeoutCount,
		RemoteCacheSameZoneTimeout:   c.cfg.RemoteCacheSameZoneTimeout,
		RemoteCacheSameRegionTimeout: c.cfg.RemoteCacheSameRegionTimeout,
		FlashHotKeyMissCount:         c.cfg.FlashHotKeyMissCount,
		FlashNodeReadRps:             c.cfg.FlashNodeReadRps,
		MaxDisableFlashGroupPercent:  c.cfg.MaxDisableFlashGroupPercent,
		FlashReadFlowLimit:           c.cfg.FlashReadFlowLimit,
		FlashWriteFlowLimit:          c.cfg.FlashWriteFlowLimit,
		FlashKeyFlowLimit:            c.cfg.FlashKeyFlowLimit,
		FlashNodeConnectionLimit:     c.cfg.FlashNodeConnectionLimit,
		RemoteClientFlowLimit:        c.cfg.RemoteClientFlowLimit,
	}
	return cv
}

func (c *Cluster) loadClusterValue() (err error) {
	result, err := c.fsm.store.SeekForPrefix([]byte(clusterPrefix))
	if err != nil {
		err = fmt.Errorf("action[loadClusterValue],err:%v", err.Error())
		return err
	}
	for _, value := range result {
		cv := &clusterValue{}
		if err = json.Unmarshal(value, cv); err != nil {
			log.LogErrorf("action[loadClusterValue], unmarshal err:%v", err.Error())
			return err
		}

		if cv.Name != c.Name {
			log.LogErrorf("action[loadClusterValue] clusterName(%v) not match loaded clusterName(%v), n loaded cluster value: %+v",
				c.Name, cv.Name, cv)
			continue
		}

		log.LogDebugf("action[loadClusterValue] loaded cluster value: %+v", cv)

		if cv.FlashNodeHandleReadTimeout == 0 {
			cv.FlashNodeHandleReadTimeout = defaultFlashNodeHandleReadTimeout
		}
		c.cfg.FlashNodeHandleReadTimeout = cv.FlashNodeHandleReadTimeout
		if cv.FlashNodeReadDataNodeTimeout == 0 {
			cv.FlashNodeReadDataNodeTimeout = defaultFlashNodeReadDataNodeTimeout
		}
		if cv.FlashHotKeyMissCount == 0 {
			cv.FlashHotKeyMissCount = defaultFlashHotKeyMissCount
		}
		if cv.FlashNodeReadRps == 0 {
			cv.FlashNodeReadRps = defaultFlashNodeReadRps
		}
		if cv.MaxDisableFlashGroupPercent == 0 {
			cv.MaxDisableFlashGroupPercent = defaultMaxDisableFlashGroupPercent
		}
		c.cfg.FlashHotKeyMissCount = cv.FlashHotKeyMissCount
		c.cfg.FlashNodeReadRps = cv.FlashNodeReadRps
		c.cfg.MaxDisableFlashGroupPercent = cv.MaxDisableFlashGroupPercent

		c.cfg.FlashReadFlowLimit = cv.FlashReadFlowLimit
		c.cfg.FlashWriteFlowLimit = cv.FlashWriteFlowLimit
		c.cfg.RemoteClientFlowLimit = cv.RemoteClientFlowLimit
		c.cfg.FlashKeyFlowLimit = cv.FlashKeyFlowLimit
		if cv.FlashNodeConnectionLimit == 0 {
			cv.FlashNodeConnectionLimit = defaultFlashNodeConnectionLimit
		}
		c.cfg.FlashNodeConnectionLimit = cv.FlashNodeConnectionLimit
		c.syncMaxDisableFlashGroupPercentToFlashTopos()

		c.cfg.FlashNodeReadDataNodeTimeout = cv.FlashNodeReadDataNodeTimeout
		log.LogInfof("action[loadClusterValue] flashNodeHandleReadTimeout %v(ms), flashNodeReadDataNodeTimeout%v(ms), flashHotKeyMissCount(%v), flashNodeReadRps(%v), maxDisableFlashGroupPercent(%v), flashReadFlowLimit(%v), flashWriteFlowLimit(%v), remoteClientFlowLimit(%v), flashKeyFlowLimit(%v), flashNodeConnectionLimit(%v)",
			cv.FlashNodeHandleReadTimeout, cv.FlashNodeReadDataNodeTimeout, cv.FlashHotKeyMissCount, cv.FlashNodeReadRps, cv.MaxDisableFlashGroupPercent, cv.FlashReadFlowLimit, cv.FlashWriteFlowLimit, cv.RemoteClientFlowLimit, cv.FlashKeyFlowLimit, cv.FlashNodeConnectionLimit)

		if cv.RemoteCacheTTL == 0 {
			cv.RemoteCacheTTL = proto.DefaultRemoteCacheTTL
		}
		c.cfg.RemoteCacheTTL = cv.RemoteCacheTTL

		if cv.RemoteCacheReadTimeout == 0 {
			cv.RemoteCacheReadTimeout = proto.DefaultRemoteCacheClientReadTimeout
		}
		c.cfg.RemoteCacheReadTimeout = cv.RemoteCacheReadTimeout
		c.cfg.RemoteCacheMultiRead = cv.RemoteCacheMultiRead

		if cv.FlashNodeTimeoutCount == 0 {
			cv.FlashNodeTimeoutCount = proto.DefaultFlashNodeTimeoutCount
		}
		c.cfg.FlashNodeTimeoutCount = cv.FlashNodeTimeoutCount

		if cv.RemoteCacheSameZoneTimeout == 0 {
			cv.RemoteCacheSameZoneTimeout = proto.DefaultRemoteCacheSameZoneTimeout
		}
		c.cfg.RemoteCacheSameZoneTimeout = cv.RemoteCacheSameZoneTimeout

		if cv.RemoteCacheSameRegionTimeout == 0 {
			cv.RemoteCacheSameRegionTimeout = proto.DefaultRemoteCacheSameRegionTimeout
		}
		c.cfg.RemoteCacheSameRegionTimeout = cv.RemoteCacheSameRegionTimeout
		log.LogInfof("action[loadClusterValue] remoteCacheTTL(%v), remoteCacheReadTimeout(%v), remoteCacheMultiRead(%v), flashNodeTimeoutCount(%v), remoteCacheSameZoneTimeout(%v), remoteCacheSameRegionTimeout(%v)",
			cv.RemoteCacheTTL, cv.RemoteCacheReadTimeout, cv.RemoteCacheMultiRead, cv.FlashNodeTimeoutCount, cv.RemoteCacheSameZoneTimeout, cv.RemoteCacheSameRegionTimeout)
	}

	return
}

func (c *Cluster) syncMaxDisableFlashGroupPercentToFlashTopos() {
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		topo.SetMaxDisableFlashGroupPercent(c.cfg.MaxDisableFlashGroupPercent)
		return true
	})
}

func (c *Cluster) defaultFlashNodeHeartbeatConfig() FlashNodeHeartbeatConfig {
	return FlashNodeHeartbeatConfig{
		FlashNodeHandleReadTimeout:   c.cfg.FlashNodeHandleReadTimeout,
		FlashNodeReadDataNodeTimeout: c.cfg.FlashNodeReadDataNodeTimeout,
		FlashHotKeyMissCount:         c.cfg.FlashHotKeyMissCount,
		FlashNodeReadRps:             c.cfg.FlashNodeReadRps,
		FlashNodeLruCapacity:         flashnode.DefaultLRUCapacity,
		FlashNodeLruFhCapacity:       flashnode.DefaultLRUFhCapacity,
		FlashReadFlowLimit:           c.cfg.FlashReadFlowLimit,
		FlashWriteFlowLimit:          c.cfg.FlashWriteFlowLimit,
		FlashKeyFlowLimit:            c.cfg.FlashKeyFlowLimit,
		FlashNodeConnectionLimit:     c.cfg.FlashNodeConnectionLimit,
	}
}

func (c *Cluster) loadFlashTopos() (err error) {
	result, err := c.fsm.store.SeekForPrefix([]byte(flashTopoPrefix))
	if err != nil {
		return fmt.Errorf("action[loadFlashTopos],err:%v", err.Error())
	}

	c.flashNodeTopo = new(sync.Map)
	if len(result) == 0 {
		if err = c.AddFlashTopo(proto.DefaultTopoName, proto.DefaultRegion); err != nil {
			return
		}
		if err = c.AddFlashTopo(proto.IdleTopoName, proto.DefaultRegion); err != nil {
			return
		}
		return nil
	}

	findIdle := false
	for _, value := range result {
		ftv := &FlashNodeTopologyValue{}
		if err = json.Unmarshal(value, ftv); err != nil {
			return fmt.Errorf("action[loadFlashTopos],value:%v,unmarshal err:%v", string(value), err)
		}
		topo := NewFlashNodeTopology(ftv.Name, ftv.Region, ftv.ID, ftv.Status)
		topo.DeleteExecTime = ftv.DeleteExecTime
		topo.DeleteStep = ftv.DeleteStep
		topo.DeleteGradualFlag = ftv.DeleteGradualFlag
		if ftv.RemoteCacheReadFlowMap != nil {
			topo.RemoteCacheReadFlowMap = ftv.RemoteCacheReadFlowMap
		}
		if ftv.RemoteCacheWriteFlowMap != nil {
			topo.RemoteCacheWriteFlowMap = ftv.RemoteCacheWriteFlowMap
		}
		topo.FlashNodeHandleReadTimeout = ftv.FlashNodeHandleReadTimeout
		topo.FlashNodeReadDataNodeTimeout = ftv.FlashNodeReadDataNodeTimeout
		topo.FlashHotKeyMissCount = ftv.FlashHotKeyMissCount
		topo.FlashNodeReadRps = ftv.FlashNodeReadRps
		topo.FlashNodeLruCapacity = ftv.FlashNodeLruCapacity
		topo.FlashNodeLruFhCapacity = ftv.FlashNodeLruFhCapacity
		topo.FlashReadFlowLimit = ftv.FlashReadFlowLimit
		topo.FlashWriteFlowLimit = ftv.FlashWriteFlowLimit
		topo.FlashKeyFlowLimit = ftv.FlashKeyFlowLimit
		topo.FlashNodeConnectionLimit = ftv.FlashNodeConnectionLimit
		topo.FillHeartbeatConfigDefaults(c.defaultFlashNodeHeartbeatConfig())
		topo.SyncFlashGroupFunc = c.syncUpdateFlashGroup
		topo.SetMaxDisableFlashGroupPercent(c.cfg.MaxDisableFlashGroupPercent)
		c.flashNodeTopo.Store(topo.Name, topo)
		if topo.Name == proto.IdleTopoName {
			findIdle = true
		}
	}
	if !findIdle {
		if err = c.AddFlashTopo(proto.IdleTopoName, proto.DefaultRegion); err != nil {
			return
		}
	}
	return nil
}

func (c *Cluster) loadFlashNodes() (err error) {
	result, err := c.fsm.store.SeekForPrefix([]byte(flashNodePrefix))
	if err != nil {
		err = fmt.Errorf("action[loadFlashNodes],err:%v", err.Error())
		return
	}

	for _, value := range result {
		fnv := &FlashNodeValue{}
		if err = json.Unmarshal(value, fnv); err != nil {
			err = fmt.Errorf("action[loadFlashNodes],value:%v,unmarshal err:%v", string(value), err)
			return
		}
		flashNode := NewFlashNodeFromFnv(c.Name, fnv)
		flashNode.ID = fnv.ID
		// load later in loadFlashTopology
		flashNode.FlashGroupID = fnv.FlashGroupID

		topoName := flashNode.FlashNodeTopoName
		if topoName == "" {
			topoName = proto.DefaultTopoName
			if flashNode.FlashGroupID == UnusedFlashNodeFlashGroupID {
				topoName = proto.IdleTopoName
			}
		}

		flashTopo, topoErr := c.PeekFlashTopo(topoName)
		if topoErr != nil {
			log.LogWarnf("action[loadFlashNodes], topo(%v) not found for flashNode(%v), fallback to default", topoName, flashNode.Addr)
			flashTopo, topoErr = c.PeekFlashTopo(proto.DefaultTopoName)
			if topoErr != nil {
				return topoErr
			}
		}

		_, err = flashTopo.GetZone(flashNode.ZoneName)
		if err != nil {
			flashTopo.PutZoneIfAbsent(NewFlashNodeZone(flashNode.ZoneName))
			err = nil
		}
		err = flashTopo.PutFlashNode(flashNode)
		if err != nil {
			log.LogWarnf("action[loadFlashNodes], flashNode[flashNodeId:%v addr:%s flashGroupId:%v topo: %v region:%v] put topo %v failed %v",
				flashNode.ID, flashNode.Addr, flashNode.FlashGroupID, flashNode.FlashNodeTopoName, flashNode.Region, flashTopo.Name, err.Error())
			idleTopo, idleErr := c.PeekFlashTopo(proto.IdleTopoName)
			if idleErr != nil {
				return idleErr
			}
			if putErr := idleTopo.PutFlashNode(flashNode); putErr != nil {
				return putErr
			}
			flashNode.FlashNodeTopoName = proto.IdleTopoName
		}
		log.LogInfof("action[loadFlashNodes], flashNode[flashNodeId:%v addr:%s flashGroupId:%v topo: %v]",
			flashNode.ID, flashNode.Addr, flashNode.FlashGroupID, flashNode.FlashNodeTopoName)
	}
	return
}

func (c *Cluster) loadFlashGroups() (err error) {
	result, err := c.fsm.store.SeekForPrefix([]byte(flashGroupPrefix))
	if err != nil {
		err = fmt.Errorf("action[loadFlashGroups],err:%v", err.Error())
		return err
	}
	for _, value := range result {
		fgv := &FlashGroupValue{}
		if err = json.Unmarshal(value, &fgv); err != nil {
			err = fmt.Errorf("action[loadFlashGroups],value:%v,unmarshal err:%v", string(value), err)
			return
		}
		flashGroup := NewFlashGroupFromFgv(fgv)
		flashTopo, topoErr := c.PeekFlashTopo(flashGroup.FlashNodeTopoName)
		if topoErr != nil {
			log.LogWarnf("action[loadFlashGroups], flashGroup(%v) topo(%v) not found", flashGroup.ID, flashGroup.FlashNodeTopoName)
			continue
		}
		if err = flashTopo.SaveFlashGroup(flashGroup); err != nil {
			return err
		}
		log.LogInfof("action[loadFlashGroups],flashGroup[%v] topo %v", flashGroup.ID, flashGroup.FlashNodeTopoName)
	}
	return
}

func (c *Cluster) loadFlashTopology() (err error) {
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		err = topo.Load()
		return err == nil
	})
	return err
}

func (m *RaftCmd) setOpType() {
	keyArr := strings.Split(m.K, keySeparator)
	if len(keyArr) < 2 {
		log.LogWarnf("action[setOpType] invalid length[%v]", keyArr)
		return
	}
	switch keyArr[1] {
	case clusterAcronym:
		m.Op = opSyncPutCluster
	case maxCommonIDKey:
		m.Op = opSyncAllocCommonID
	default:
		log.LogWarnf("action[setOpType] unknown opCode[%v]", keyArr[1])
	}
}

func (c *Cluster) submit(metadata *RaftCmd) (err error) {
	cmd, err := metadata.Marshal()
	if err != nil {
		return errors.New(err.Error())
	}
	if _, err = c.partition.Submit(cmd); err != nil {
		msg := fmt.Sprintf("action[metadata_submit] err:%v", err.Error())
		return errors.New(msg)
	}
	return
}

func (c *Cluster) syncPutCluster() (err error) {
	metadata := new(RaftCmd)
	metadata.Op = opSyncPutCluster
	metadata.K = clusterPrefix + c.Name
	cv := newClusterValue(c)
	log.LogInfof("action[syncPutCluster] cluster value:[%+v]", cv)
	metadata.V, err = json.Marshal(cv)
	if err != nil {
		return
	}
	return c.submit(metadata)
}
