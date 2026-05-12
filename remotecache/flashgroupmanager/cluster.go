package flashgroupmanager

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

type Cluster struct {
	Name          string
	CreateTime    int64
	flashNodeTopo *sync.Map
	idAlloc       *IDAllocator
	stopc         chan bool
	stopFlag      int32
	wg            sync.WaitGroup
	cfg           *clusterConfig
	leaderInfo    *LeaderInfo
	fsm           *MetadataFsm
	partition     raftstore.Partition
}

func (c *Cluster) initDefaultFlashTopos() {
	c.flashNodeTopo = new(sync.Map)
}

func newCluster(name string, cfg *clusterConfig, leaderInfo *LeaderInfo, fsm *MetadataFsm, partition raftstore.Partition) (c *Cluster) {
	c = new(Cluster)
	c.Name = name
	c.stopc = make(chan bool)
	c.fsm = fsm
	c.partition = partition
	c.cfg = cfg
	c.leaderInfo = leaderInfo
	c.idAlloc = newIDAllocator(c.fsm.store, c.partition)
	c.initDefaultFlashTopos()
	return
}

func (c *Cluster) createFlashGroup(setSlots []uint32, setWeight uint32, gradualFlag bool, step uint32, topoName string) (fg *FlashGroup, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[addFlashGroup],clusterID[%v] err:%v ", c.Name, err.Error())
		}
	}()
	id, err := c.idAlloc.allocateCommonID()
	if err != nil {
		return
	}
	flashTopo, err := c.PeekFlashTopo(topoName)
	if err != nil {
		return nil, err
	}
	fg, err = flashTopo.CreateFlashGroup(id, c.syncUpdateFlashGroup, c.syncAddFlashGroup, setSlots, setWeight, gradualFlag, step)
	log.LogInfof("action[addFlashGroup],clusterID[%v] id:%v Weight:%v Slots:%v success", c.Name, fg.ID, fg.Weight, fg.GetSlots())
	return
}

func (c *Cluster) addFlashNode(topoName, nodeAddr, zoneName, version, region string, id uint64) (nodeID uint64, assignedTopoName string, err error) {
	var flashNode *FlashNode
	assignedTopoName = topoName

	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		if id == 0 {
			flashNode, _ = topo.PeekFlashNode(nodeAddr)
		} else {
			flashNode, _ = topo.PeekFlashNodeById(id)
		}
		if flashNode == nil {
			return true
		}
		if flashNode.Region != region {
			err = fmt.Errorf("region is conflict: [%v] previously registered[%v]", region, flashNode.Region)
		}
		assignedTopoName = topo.Name
		return false
	})
	if err != nil {
		return
	}

	flashTopo, err := c.PeekFlashTopo(assignedTopoName)
	if err != nil {
		return
	}
	nodeID, err = flashTopo.AddFlashNode(c.Name, nodeAddr, zoneName, version, region, id,
		c.idAlloc.allocateCommonID, c.syncAddFlashNode, c.syncMoveFlashNode)
	return
}

func (c *Cluster) updateFlashNode(flashTopo *FlashNodeTopology, flashNode *FlashNode, enable bool) (err error) {
	return flashTopo.UpdateFlashNode(flashNode, enable, c.syncUpdateFlashNode)
}

func (c *Cluster) updateFlashNodeWorkRole(flashNode *FlashNode, workRole string) error {
	flashNode.Lock()
	defer flashNode.Unlock()
	flashNode.WorkRole = workRole
	if err := c.syncUpdateFlashNode(flashNode); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) removeFlashNode(flashTopo *FlashNodeTopology, flashNode *FlashNode) (err error) {
	return flashTopo.RemoveFlashNode(c.Name, flashNode, c.syncDeleteFlashNode)
}

func (c *Cluster) scheduleToUpdateFlashGroupRespCache() {
	go func() {
		dur := time.Second * time.Duration(5)
		ticker := time.NewTicker(dur)
		defer ticker.Stop()
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.flashNodeTopo.Range(func(_, value interface{}) bool {
					topo, ok := value.(*FlashNodeTopology)
					if !ok || topo == nil {
						return true
					}
					topo.UpdateClientResponse()
					return true
				})
			}
			select {
			case <-c.stopc:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *Cluster) scheduleTask() {
	c.scheduleToUpdateFlashGroupRespCache()
	c.scheduleToCheckHeartbeat()
	c.scheduleToUpdateFlashGroupSlots()
}

func (c *Cluster) PeekFlashTopo(name string) (flashTopo *FlashNodeTopology, err error) {
	value, ok := c.flashNodeTopo.Load(name)
	if !ok {
		err = errors.Trace(notFoundMsg(fmt.Sprintf("flashTopo[%v]", name)), "")
		return
	}
	flashTopo = value.(*FlashNodeTopology)
	return
}

func (c *Cluster) PeekFlashTopoByFgId(fgID uint64) (flashTopo *FlashNodeTopology, err error) {
	if fgID == 0 {
		return nil, errors.NewErrorf("fg id is 0")
	}
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		if _, e := topo.GetFlashGroup(fgID); e == nil {
			flashTopo = topo
			return false
		}
		return true
	})
	if flashTopo == nil {
		err = errors.Trace(notFoundMsg(fmt.Sprintf("flashTopo by fgId[%v]", fgID)), "")
	}
	return
}

func (c *Cluster) ListAllFlashTopos() (views []*proto.FlashTopologyAdminView) {
	views = make([]*proto.FlashTopologyAdminView, 0)
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		if view := topo.GetFlashTopoAdminView(); view != nil {
			views = append(views, view)
		}
		return true
	})
	return
}

func (c *Cluster) AddFlashTopo(name, region string) (err error) {
	var id uint64
	if c.idAlloc == nil {
		return fmt.Errorf("cluster is not initialized")
	}
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		return
	}
	topo := NewFlashNodeTopology(name, region, id, proto.TopoStatusNormal)
	topo.SetHeartbeatConfig(c.defaultFlashNodeHeartbeatConfig())
	topo.SyncFlashGroupFunc = c.syncUpdateFlashGroup
	topo.SetMaxDisableFlashGroupPercent(c.cfg.MaxDisableFlashGroupPercent)
	c.flashNodeTopo.Store(name, topo)
	if err = c.syncAddFlashTopo(topo); err != nil {
		c.flashNodeTopo.Delete(name)
	}
	return
}

func (c *Cluster) DelFlashTopo(name string, gradualFlag bool, step uint32) (err error) {
	srcTopo, err := c.PeekFlashTopo(name)
	if err != nil {
		return
	}
	idleTopo, err := c.PeekFlashTopo(proto.IdleTopoName)
	if err != nil {
		return
	}
	if err = srcTopo.DeleteAllFlashGroups(c.Name, idleTopo, gradualFlag, step,
		c.syncUpdateFlashGroup, c.syncUpdateFlashNode, c.syncDeleteFlashGroup,
		c.syncDeleteFlashNode, c.syncAddFlashNode, c.syncMoveFlashNode); err != nil {
		return
	}
	c.flashNodeTopo.Delete(name)
	return c.syncDeleteFlashTopo(srcTopo)
}

func (c *Cluster) RenameFlashNodeTopo(srcTop *FlashNodeTopology, newName string) (err error) {
	oldName := srcTop.Name
	if err = srcTop.Rename(newName, c.syncUpdateFlashNode, c.syncUpdateFlashGroup); err != nil {
		return
	}
	srcTop.Name = newName
	c.flashNodeTopo.Delete(oldName)
	c.flashNodeTopo.Store(newName, srcTop)
	return c.syncUpdateFlashTopo(srcTop)
}

func (c *Cluster) RemoveFlashNodesFromFlashGroup(srcTop, idleTop *FlashNodeTopology, flashGroupID uint64,
	addr string, zoneName string, count int,
) (flashGroup *FlashGroup, err error) {
	if flashGroup, err = srcTop.GetFlashGroup(flashGroupID); err != nil {
		return
	}

	if addr != "" {
		var fn *FlashNode
		if fn, err = srcTop.PeekFlashNode(addr); err != nil {
			return
		}
		if err = srcTop.ChangeFlashNodeTopo(c.Name, idleTop, fn, c.syncDeleteFlashNode, c.syncAddFlashNode, c.syncMoveFlashNode); err != nil {
			return
		}
		return
	}

	flashNodeHosts := flashGroup.GetTargetZoneFlashNodeHosts(zoneName)
	if len(flashNodeHosts) < count {
		return nil, fmt.Errorf("flashNodeHostsCount:%v less than expectCount:%v,flashNodeHosts:%v", len(flashNodeHosts), count, flashNodeHosts)
	}
	for _, host := range flashNodeHosts[:count] {
		fn, e := srcTop.PeekFlashNode(host)
		if e != nil {
			return nil, e
		}
		if err = srcTop.ChangeFlashNodeTopo(c.Name, idleTop, fn, c.syncDeleteFlashNode, c.syncAddFlashNode, c.syncMoveFlashNode); err != nil {
			return nil, err
		}
	}
	return
}

func (c *Cluster) peekFlashNode(topoName, addr string) (flashNode *FlashNode, err error) {
	flashTopo, err := c.PeekFlashTopo(topoName)
	if err != nil {
		return nil, err
	}
	value, ok := flashTopo.flashNodeMap.Load(addr)
	if !ok {
		err = errors.Trace(notFoundMsg(fmt.Sprintf("flashnode[%v]", addr)), "")
		return
	}
	flashNode = value.(*FlashNode)
	return
}

func (c *Cluster) handleFlashNodeTaskResponse(nodeAddr string, task *proto.AdminTask) {
	if task == nil {
		log.LogInfof("flash action[handleFlashNodeTaskResponse] receive addr[%v] task response, but task is nil", nodeAddr)
		return
	}
	log.LogInfof("flash action[handleFlashNodeTaskResponse] receive addr[%v] task: %v", nodeAddr, task.ToString())
	var (
		err       error
		flashNode *FlashNode
	)

	topoName := task.TopoName
	if topoName == "" {
		topoName = proto.DefaultTopoName
	}
	if flashNode, err = c.peekFlashNode(topoName, nodeAddr); err != nil {
		goto errHandler
	}
	flashNode.TaskManager.DelTask(task)
	if err = unmarshalTaskResponse(task); err != nil {
		goto errHandler
	}

	switch task.OpCode {
	case proto.OpFlashNodeHeartbeat:
		response := task.Response.(*proto.FlashNodeHeartbeatResponse)
		err = c.handleFlashNodeHeartbeatResp(topoName, task.OperatorAddr, response)
	default:
		err = fmt.Errorf("flash unknown operate code %v", task.OpCode)
		goto errHandler
	}

	if err != nil {
		goto errHandler
	}
	return

errHandler:
	log.LogWarnf("flash handleFlashNodeTaskResponse failed, task: %v, err: %v", task.ToString(), err)
}

func (c *Cluster) handleFlashNodeHeartbeatResp(topoName, nodeAddr string, resp *proto.FlashNodeHeartbeatResponse) (err error) {
	if resp.Status != proto.TaskSucceeds {
		Warn(c.Name, fmt.Sprintf("action[handleFlashNodeHeartbeatResp] clusterID[%v] flashNode[%v] heartbeat task failed, err[%v]",
			c.Name, nodeAddr, resp.Result))
		return
	}
	var node *FlashNode
	if node, err = c.peekFlashNode(topoName, nodeAddr); err != nil {
		log.LogErrorf("action[handleFlashNodeHeartbeatResp], flashNode[%v], heartbeat error: %v", nodeAddr, err.Error())
		return
	}
	node.SetActive()
	node.UpdateFlashNodeStatHeartbeat(resp)
	return
}

func (c *Cluster) checkFlashNodeHeartbeat() {
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		tasks := topo.CreateFlashNodeHeartBeatTasks(c.masterAddr(), nil, nil, nil)
		c.addFlashNodeHeartbeatTasks(topo.Name, tasks)
		return true
	})
}

func (c *Cluster) addFlashNodeHeartbeatTasks(topoName string, tasks []*proto.AdminTask) {
	for _, t := range tasks {
		if t == nil {
			continue
		}
		node, err := c.peekFlashNode(topoName, t.OperatorAddr)
		if err != nil {
			log.LogWarn(fmt.Sprintf("action[syncFlashNodeHeartbeatTasks],nodeAddr:%v,taskID:%v,err:%v", t.OperatorAddr, t.ID, err.Error()))
			continue
		}
		t.TopoName = topoName
		node.TaskManager.AddTask(t)
	}
}

func (c *Cluster) scheduleToCheckHeartbeat() {
	c.runTask(
		&cTask{
			tickTime: time.Second * defaultIntervalToCheckHeartbeat,
			name:     "scheduleToCheckHeartbeat_checkFlashNodeHeartbeat",
			function: func() (fin bool) {
				if c.partition != nil && c.partition.IsRaftLeader() {
					c.checkFlashNodeHeartbeat()
				}
				return
			},
		})
}

func (c *Cluster) scheduleToUpdateFlashGroupSlots() {
	c.runTask(
		&cTask{
			tickTime: time.Minute,
			name:     "scheduleToUpdateFlashGroupSlots",
			function: func() (fin bool) {
				if c.partition != nil && c.partition.IsRaftLeader() {
					idleTopo, err := c.PeekFlashTopo(proto.IdleTopoName)
					if err != nil {
						log.LogWarnf("scheduleToUpdateFlashGroupSlots peek idle topo failed: %v", err)
						return
					}
					c.flashNodeTopo.Range(func(_, value interface{}) bool {
						topo, ok := value.(*FlashNodeTopology)
						if !ok || topo == nil {
							return true
						}
						topo.UpdateFlashGroupSlots(c.Name, idleTopo, c.syncDeleteFlashGroup, c.syncUpdateFlashGroup,
							c.syncUpdateFlashNode, c.syncDeleteFlashNode, c.syncAddFlashNode, c.syncMoveFlashNode)
						return true
					})
				}
				return
			},
		})
}

func getNewSlots(slots []uint32, pendingSlots []uint32, flag proto.SlotStatus) (newSlots []uint32) {
	if flag == proto.SlotStatus_Creating { // expand flashGroup slots
		newSlots = append(slots, pendingSlots...)
		sort.Slice(newSlots, func(i, j int) bool { return newSlots[i] < newSlots[j] })
		return
	} else { // shrink flashGroup slots
		slotMap := make(map[uint32]struct{})
		for _, val := range pendingSlots {
			if _, ok := slotMap[val]; !ok {
				slotMap[val] = struct{}{}
			}
		}
		for _, val := range slots {
			if _, ok := slotMap[val]; !ok {
				newSlots = append(newSlots, val)
			}
		}
		return
	}
}

type cTask struct {
	name     string
	tickTime time.Duration
	function func() bool
	noWait   bool
}

func (c *Cluster) runTask(task *cTask) {
	if !task.noWait {
		c.wg.Add(1)
	}
	go func() {
		if !task.noWait {
			defer c.wg.Done()
		}
		log.LogWarnf("runTask %v start!", task.name)
		currTickTm := task.tickTime
		ticker := time.NewTicker(currTickTm)
		for {
			select {
			case <-ticker.C:
				if task.function() {
					log.LogWarnf("runTask %v exit!", task.name)
					ticker.Stop()
					return
				}
				if currTickTm != task.tickTime { // there's no conflict, thus no need consider consistency between tickTime and currTickTm
					ticker.Reset(task.tickTime)
					currTickTm = task.tickTime
				}
			case <-c.stopc:
				log.LogWarnf("runTask %v exit!", task.name)
				ticker.Stop()
				return
			}
		}
	}()
}

func (c *Cluster) masterAddr() (addr string) {
	return c.leaderInfo.addr
}

func (c *Cluster) allMasterNodes() (masterNodes []proto.NodeView) {
	masterNodes = make([]proto.NodeView, 0)

	for _, addr := range c.cfg.peerAddrs {
		split := strings.Split(addr, colonSplit)
		id, _ := strconv.ParseUint(split[0], 10, 64)
		masterNode := proto.NodeView{ID: id, Addr: split[1] + ":" + split[2], Status: true}
		masterNodes = append(masterNodes, masterNode)
	}
	return masterNodes
}

func (c *Cluster) allFlashNodes() (flashNodes []proto.NodeView) {
	flashNodes = make([]proto.NodeView, 0)
	c.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		flashNodes = append(flashNodes, topo.GetAllFlashNodesView()...)
		return true
	})
	return
}

func (c *Cluster) syncAddFlashGroup(flashGroup *FlashGroup) (err error) {
	return c.syncPutFlashGroupInfo(opSyncAddFlashGroup, flashGroup)
}

func (c *Cluster) syncDeleteFlashGroup(flashGroup *FlashGroup) (err error) {
	return c.syncPutFlashGroupInfo(opSyncDeleteFlashGroup, flashGroup)
}

func (c *Cluster) syncUpdateFlashGroup(flashGroup *FlashGroup) (err error) {
	return c.syncPutFlashGroupInfo(opSyncUpdateFlashGroup, flashGroup)
}

func (c *Cluster) syncPutFlashGroupInfo(opType uint32, flashGroup *FlashGroup) (err error) {
	metadata := new(RaftCmd)
	metadata.Op = opType
	metadata.K = flashGroupPrefix + strconv.FormatUint(flashGroup.ID, 10)
	metadata.V, err = json.Marshal(flashGroup.FlashGroupValue)
	if err != nil {
		return errors.New(err.Error())
	}
	return c.submit(metadata)
}

func (c *Cluster) syncPutFlashNodeInfo(opType uint32, flashNode *FlashNode) (err error) {
	metadata := new(RaftCmd)
	metadata.Op = opType
	metadata.K = flashNodePrefix + strconv.FormatUint(flashNode.ID, 10) + keySeparator + flashNode.Addr
	metadata.V, err = json.Marshal(flashNode.FlashNodeValue)
	if err != nil {
		return errors.New(err.Error())
	}
	return c.submit(metadata)
}

func (c *Cluster) syncAddFlashNode(flashNode *FlashNode) (err error) {
	return c.syncPutFlashNodeInfo(opSyncAddFlashNode, flashNode)
}

func (c *Cluster) syncDeleteFlashNode(flashNode *FlashNode) (err error) {
	return c.syncPutFlashNodeInfo(opSyncDeleteFlashNode, flashNode)
}

func (c *Cluster) syncUpdateFlashNode(flashNode *FlashNode) (err error) {
	return c.syncPutFlashNodeInfo(opSyncUpdateFlashNode, flashNode)
}

func (c *Cluster) syncAddFlashTopo(flashTopo *FlashNodeTopology) (err error) {
	return c.syncPutFlashTopoInfo(opSyncAddFlashTopo, flashTopo)
}

func (c *Cluster) syncUpdateFlashTopo(flashTopo *FlashNodeTopology) (err error) {
	return c.syncPutFlashTopoInfo(opSyncUpdateFlashTopo, flashTopo)
}

func (c *Cluster) syncDeleteFlashTopo(flashTopo *FlashNodeTopology) (err error) {
	return c.syncPutFlashTopoInfo(opSyncDeleteFlashTopo, flashTopo)
}

func (c *Cluster) syncPutFlashTopoInfo(opType uint32, flashTopo *FlashNodeTopology) (err error) {
	metadata := new(RaftCmd)
	metadata.Op = opType
	metadata.K = flashTopoPrefix + strconv.FormatUint(flashTopo.ID, 10) + keySeparator
	metadata.V, err = json.Marshal(flashTopo.FlashNodeTopologyValue)
	if err != nil {
		return errors.New(err.Error())
	}
	return c.submit(metadata)
}

func (c *Cluster) syncMoveFlashNode(oldAddr string, newValue *FlashNodeValue) (err error) {
	if newValue == nil {
		return fmt.Errorf("syncMoveFlashNode: newValue is nil")
	}
	oldKey := flashNodePrefix + strconv.FormatUint(newValue.ID, 10) + keySeparator + oldAddr
	newKey := flashNodePrefix + strconv.FormatUint(newValue.ID, 10) + keySeparator + newValue.Addr
	newV, err := json.Marshal(*newValue)
	if err != nil {
		return errors.New(err.Error())
	}
	mv := &moveKeyValueCmd{NewK: newKey, NewV: newV}
	mvBytes, err := json.Marshal(mv)
	if err != nil {
		return errors.New(err.Error())
	}
	metadata := &RaftCmd{Op: opSyncMoveFlashNode, K: oldKey, V: mvBytes}
	return c.submit(metadata)
}

func (c *Cluster) tryToChangeLeaderByHost() error {
	return c.partition.TryToLeader(1)
}

func (c *Cluster) syncFlashNodeSetIOLimitTasks(tasks []*proto.AdminTask) {
	for _, t := range tasks {
		if t == nil {
			continue
		}
		topoName := t.TopoName
		if topoName == "" {
			topoName = proto.DefaultTopoName
		}
		node, err := c.peekFlashNode(topoName, t.OperatorAddr)
		if err != nil {
			log.LogWarn(fmt.Sprintf("action[syncFlashNodeHeartbeatTasks],nodeAddr:%v,taskID:%v,err:%v", t.OperatorAddr, t.ID, err.Error()))
			continue
		}
		if _, err = node.SyncSendAdminTask(t); err != nil {
			log.LogWarn(fmt.Sprintf("action[syncFlashNodeHeartbeatTasks],nodeAddr:%v,taskID:%v,err:%v", t.OperatorAddr, t.ID, err.Error()))
			continue
		}
	}
}
