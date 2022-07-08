// Copyright 2022 The CubeFS Authors.
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

package scheduler

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/taskswitch"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/scheduler/base"
	"github.com/cubefs/cubefs/blobstore/scheduler/client"
	"github.com/cubefs/cubefs/blobstore/scheduler/db"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

// IBalancer define the interface of balance manager
type IBalancer interface {
	Migrator
}

const (
	collectBalanceTaskPauseS = 5
)

var (
	// ErrNoBalanceVunit no balance volume unit on disk
	ErrNoBalanceVunit = errors.New("no balance volume unit on disk")
	// ErrTooManyBalancingTasks too many balancing tasks
	ErrTooManyBalancingTasks = errors.New("too many balancing tasks")
)

// BalanceMgrConfig balance task manager config
type BalanceMgrConfig struct {
	BalanceDiskCntLimit int   `json:"balance_disk_cnt_limit"`
	MaxDiskFreeChunkCnt int64 `json:"max_disk_free_chunk_cnt"`
	MinDiskFreeChunkCnt int64 `json:"min_disk_free_chunk_cnt"`
	MigrateConfig
}

// BalanceMgr balance manager
type BalanceMgr struct {
	IMigrater

	clusterTopology IClusterTopology
	clusterMgrCli   client.ClusterMgrAPI

	cfg *BalanceMgrConfig
}

// NewBalanceMgr returns balance manager
func NewBalanceMgr(
	clusterMgrCli client.ClusterMgrAPI,
	volumeUpdater client.IVolumeUpdater,
	taskSwitch taskswitch.ISwitcher,
	clusterTopology IClusterTopology,
	taskTbl db.IMigrateTaskTable,
	conf *BalanceMgrConfig) *BalanceMgr {
	mgr := &BalanceMgr{
		clusterTopology: clusterTopology,
		clusterMgrCli:   clusterMgrCli,
		cfg:             conf,
	}
	mgr.IMigrater = NewMigrateMgr(clusterMgrCli, volumeUpdater, taskSwitch, taskTbl,
		&conf.MigrateConfig, proto.BalanceTaskType, conf.ClusterID)
	mgr.IMigrater.SetLockFailHandleFunc(mgr.IMigrater.FinishTaskInAdvanceWhenLockFail)
	return mgr
}

// Run run balance task manager
func (mgr *BalanceMgr) Run() {
	go mgr.collectTaskLoop()
	mgr.IMigrater.Run()
	go mgr.clearTaskLoop()
}

// Close close balance task manager
func (mgr *BalanceMgr) Close() {
	mgr.clusterTopology.Close()
	mgr.IMigrater.Close()
}

func (mgr *BalanceMgr) collectTaskLoop() {
	t := time.NewTicker(time.Duration(mgr.cfg.CollectTaskIntervalS) * time.Second)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			mgr.IMigrater.WaitEnable()
			err := mgr.collectionTask()
			if err == ErrTooManyBalancingTasks || err == ErrNoBalanceVunit {
				log.Debugf("no task to collect and sleep: sleep second[%d], err[%+v]", collectBalanceTaskPauseS, err)
				time.Sleep(time.Duration(collectBalanceTaskPauseS) * time.Second)
			}
		case <-mgr.IMigrater.Done():
			return
		}
	}
}

func (mgr *BalanceMgr) collectionTask() (err error) {
	span, ctx := trace.StartSpanFromContext(context.Background(), "balance_collectionTask")
	defer span.Finish()

	needBalanceDiskCnt := mgr.cfg.BalanceDiskCntLimit - mgr.IMigrater.GetMigratingDiskNum()
	if needBalanceDiskCnt <= 0 {
		span.Warnf("the number of balancing disk is greater than config: current[%d], conf[%d]",
			mgr.IMigrater.GetMigratingDiskNum(), mgr.cfg.BalanceDiskCntLimit)
		return ErrTooManyBalancingTasks
	}

	// select balance disks
	disks := mgr.selectDisks(mgr.cfg.MaxDiskFreeChunkCnt, mgr.cfg.MinDiskFreeChunkCnt)
	span.Debugf("select balance disks: len[%d]", len(disks))

	balanceDiskCnt := 0
	for _, disk := range disks {
		err = mgr.genOneBalanceTask(ctx, disk)
		if err != nil {
			continue
		}

		balanceDiskCnt++
		if balanceDiskCnt >= needBalanceDiskCnt {
			break
		}
	}
	// if balanceDiskCnt==0, means there is no balance volume unit on disk and need to do collect task later
	if balanceDiskCnt == 0 {
		span.Infof("select disks has no balance volume unit on disk: len[%d]", len(disks))
		return ErrNoBalanceVunit
	}

	return nil
}

func (mgr *BalanceMgr) selectDisks(maxFreeChunkCnt, minFreeChunkCnt int64) []*client.DiskInfoSimple {
	var allDisks []*client.DiskInfoSimple
	for idcName := range mgr.clusterTopology.GetIDCs() {
		if idcDisks := mgr.clusterTopology.GetIDCDisks(idcName); idcDisks != nil {
			if freeChunkCntMax(idcDisks) >= maxFreeChunkCnt {
				allDisks = append(allDisks, idcDisks...)
			}
		}
	}

	var selected []*client.DiskInfoSimple
	for _, disk := range allDisks {
		if !disk.IsHealth() {
			continue
		}
		if ok := mgr.IMigrater.IsMigratingDisk(disk.DiskID); ok {
			continue
		}
		if disk.FreeChunkCnt < minFreeChunkCnt {
			selected = append(selected, disk)
		}
	}
	return selected
}

func (mgr *BalanceMgr) genOneBalanceTask(ctx context.Context, diskInfo *client.DiskInfoSimple) (err error) {
	span := trace.SpanFromContextSafe(ctx)

	vuid, err := mgr.selectBalanceVunit(ctx, diskInfo.DiskID)
	if err != nil {
		span.Errorf("generate task source failed: disk_id[%d], err[%+v]", diskInfo.DiskID, err)
		return
	}

	span.Debugf("select balance volume unit; vuid[%d+, volume_id[%v]", vuid, vuid.Vid())
	task := &proto.MigrateTask{
		TaskID:       mgr.genUniqTaskID(vuid.Vid()),
		State:        proto.MigrateStateInited,
		SourceIdc:    diskInfo.Idc,
		SourceDiskID: diskInfo.DiskID,
		SourceVuid:   vuid,
	}
	mgr.IMigrater.AddTask(ctx, task)
	return
}

func (mgr *BalanceMgr) selectBalanceVunit(ctx context.Context, diskID proto.DiskID) (vuid proto.Vuid, err error) {
	span := trace.SpanFromContextSafe(ctx)

	vunits, err := mgr.clusterMgrCli.ListDiskVolumeUnits(ctx, diskID)
	if err != nil {
		return
	}

	sortVunitByUsed(vunits)

	for i := range vunits {
		volInfo, err := mgr.clusterMgrCli.GetVolumeInfo(ctx, vunits[i].Vuid.Vid())
		if err != nil {
			span.Errorf("get volume info failed: vid[%d], err[%+v]", vunits[i].Vuid.Vid(), err)
			continue
		}
		if volInfo.IsIdle() {
			return vunits[i].Vuid, nil
		}
	}
	return vuid, ErrNoBalanceVunit
}

func (mgr *BalanceMgr) clearTaskLoop() {
	t := time.NewTicker(time.Duration(mgr.cfg.CheckTaskIntervalS) * time.Second)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			mgr.IMigrater.WaitEnable()
			mgr.ClearFinishedTask()
		case <-mgr.IMigrater.Done():
			return
		}
	}
}

// ClearFinishedTask clear finished balance task
func (mgr *BalanceMgr) ClearFinishedTask() {
	span, ctx := trace.StartSpanFromContext(context.Background(), "balance_ClearFinishedTask")
	defer span.Finish()

	clearStates := []proto.MigrateState{proto.MigrateStateFinished, proto.MigrateStateFinishedInAdvance}
	mgr.IMigrater.ClearTasksByStates(ctx, clearStates)
}

func (mgr *BalanceMgr) genUniqTaskID(vid proto.Vid) string {
	return base.GenTaskID("balance", vid)
}

func sortVunitByUsed(vunits []*client.VunitInfoSimple) {
	sort.Slice(vunits, func(i, j int) bool {
		return vunits[i].Used < vunits[j].Used
	})
}

func freeChunkCntMax(disks []*client.DiskInfoSimple) int64 {
	var max int64
	for _, disk := range disks {
		if disk.FreeChunkCnt > max {
			max = disk.FreeChunkCnt
		}
	}
	return max
}
