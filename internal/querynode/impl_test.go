// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package querynode

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"

	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/planpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	queryPb "github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/util/commonpbutil"
	"github.com/milvus-io/milvus/internal/util/concurrency"
	"github.com/milvus-io/milvus/internal/util/etcd"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

func TestImpl_GetComponentStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	node.session.UpdateRegistered(true)

	rsp, err := node.GetComponentStates(ctx)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

	node.UpdateStateCode(commonpb.StateCode_Abnormal)
	rsp, err = node.GetComponentStates(ctx)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

	node.stateCode = atomic.Value{}
	node.stateCode.Store("invalid")
	rsp, err = node.GetComponentStates(ctx)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, rsp.Status.ErrorCode)
}

func TestImpl_GetTimeTickChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	rsp, err := node.GetTimeTickChannel(ctx)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)
}

func TestImpl_GetStatisticsChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	rsp, err := node.GetStatisticsChannel(ctx)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)
}

func TestImpl_WatchDmChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	t.Run("normal_run", func(t *testing.T) {
		schema := genTestCollectionSchema()
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_WatchDmChannels,
				MsgID:    rand.Int63(),
				TargetID: node.session.ServerID,
			},
			NodeID:       0,
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			Schema:       schema,
			Infos: []*datapb.VchannelInfo{
				{
					CollectionID: 1000,
					ChannelName:  "1000-dmc0",
				},
			},
			LoadMeta: &querypb.LoadMetaInfo{
				MetricType: defaultMetricType,
			},
		}

		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)

		originPool := node.taskPool
		defer func() {
			node.taskPool = originPool
		}()
		node.taskPool, _ = concurrency.NewPool(runtime.GOMAXPROCS(0), concurrency.WithPreAlloc(true))
		node.taskPool.Release()
		status, err = node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, status.ErrorCode)
	})

	t.Run("target not match", func(t *testing.T) {
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_WatchDmChannels,
				MsgID:    rand.Int63(),
				TargetID: -1,
			},
		}
		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, status.ErrorCode)
	})

	t.Run("server unhealthy", func(t *testing.T) {
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchDmChannels,
				MsgID:   rand.Int63(),
			},
		}
		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		defer node.UpdateStateCode(commonpb.StateCode_Healthy)
		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
	})

	t.Run("server stopping", func(t *testing.T) {
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchDmChannels,
				MsgID:   rand.Int63(),
			},
		}
		node.UpdateStateCode(commonpb.StateCode_Stopping)
		defer node.UpdateStateCode(commonpb.StateCode_Healthy)
		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
	})

	t.Run("mock release after loaded", func(t *testing.T) {
		mockTSReplica := &MockTSafeReplicaInterface{}

		oldTSReplica := node.tSafeReplica
		defer func() {
			node.tSafeReplica = oldTSReplica
		}()
		node.tSafeReplica = mockTSReplica
		mockTSReplica.On("addTSafe", mock.Anything).Run(func(_ mock.Arguments) {
			node.ShardClusterService.releaseShardCluster("1001-dmc0")
		})
		schema := genTestCollectionSchema()
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_WatchDmChannels,
				MsgID:    rand.Int63(),
				TargetID: node.session.ServerID,
			},
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			Schema:       schema,
			Infos: []*datapb.VchannelInfo{
				{
					CollectionID: 1001,
					ChannelName:  "1001-dmc0",
				},
			},
			LoadMeta: &querypb.LoadMetaInfo{
				MetricType: defaultMetricType,
			},
		}

		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, status.ErrorCode)
	})
}

func TestImpl_UnsubDmChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	t.Run("normal run", func(t *testing.T) {
		schema := genTestCollectionSchema()
		req := &queryPb.WatchDmChannelsRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_WatchDmChannels,
				MsgID:    rand.Int63(),
				TargetID: node.session.ServerID,
			},
			NodeID:       0,
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			Schema:       schema,
			Infos: []*datapb.VchannelInfo{
				{
					CollectionID: 1000,
					ChannelName:  Params.CommonCfg.RootCoordDml + "-dmc0",
				},
			},
			LoadMeta: &querypb.LoadMetaInfo{
				MetricType: defaultMetricType,
			},
		}

		status, err := node.WatchDmChannels(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)

		{
			req := &queryPb.UnsubDmChannelRequest{
				Base: &commonpb.MsgBase{
					MsgType:  commonpb.MsgType_UnsubDmChannel,
					MsgID:    rand.Int63(),
					TargetID: node.session.ServerID,
				},
				NodeID:       0,
				CollectionID: defaultCollectionID,
				ChannelName:  Params.CommonCfg.RootCoordDml + "-dmc0",
			}
			originMetaReplica := node.metaReplica
			node.metaReplica = newMockReplicaInterface()
			status, err := node.UnsubDmChannel(ctx, req)
			assert.NoError(t, err)
			assert.Equal(t, commonpb.ErrorCode_UnexpectedError, status.ErrorCode)

			node.metaReplica = originMetaReplica
			status, err = node.UnsubDmChannel(ctx, req)
			assert.NoError(t, err)
			assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
		}
	})

	t.Run("target not match", func(t *testing.T) {
		req := &queryPb.UnsubDmChannelRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_UnsubDmChannel,
				MsgID:    rand.Int63(),
				TargetID: -1,
			},
		}
		status, err := node.UnsubDmChannel(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, status.ErrorCode)
	})

	t.Run("server unhealthy", func(t *testing.T) {
		req := &queryPb.UnsubDmChannelRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_UnsubDmChannel,
				MsgID:   rand.Int63(),
			},
		}
		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		status, err := node.UnsubDmChannel(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
	})
}

func TestImpl_LoadSegments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	schema := genTestCollectionSchema()

	req := &queryPb.LoadSegmentsRequest{
		Base: &commonpb.MsgBase{
			MsgType:  commonpb.MsgType_WatchQueryChannels,
			MsgID:    rand.Int63(),
			TargetID: node.session.ServerID,
		},
		DstNodeID: 0,
		Schema:    schema,
	}

	t.Run("normal run", func(t *testing.T) {
		status, err := node.LoadSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	})

	t.Run("segment already loaded", func(t *testing.T) {
		node.metaReplica.addCollection(1, schema)
		err := node.metaReplica.addPartition(1, 0)
		assert.NoError(t, err)
		err = node.metaReplica.addSegment(1, 1, 1, "channel-1", 0, nil, commonpb.SegmentState_Sealed)
		assert.NoError(t, err)
		req.Infos = []*queryPb.SegmentLoadInfo{
			{
				SegmentID:    1,
				PartitionID:  1,
				CollectionID: 1,
			},
		}
		req.NeedTransfer = false
		status, err := node.LoadSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	})

	t.Run("target not match", func(t *testing.T) {
		req := &queryPb.LoadSegmentsRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_WatchQueryChannels,
				MsgID:    rand.Int63(),
				TargetID: -1,
			},
		}
		status, err := node.LoadSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, status.ErrorCode)
	})

	t.Run("server unhealthy", func(t *testing.T) {
		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		defer node.UpdateStateCode(commonpb.StateCode_Healthy)
		status, err := node.LoadSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
	})

	t.Run("server stopping", func(t *testing.T) {
		node.UpdateStateCode(commonpb.StateCode_Stopping)
		defer node.UpdateStateCode(commonpb.StateCode_Healthy)
		status, err := node.LoadSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
	})
}

func TestImpl_ReleaseCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	req := &queryPb.ReleaseCollectionRequest{
		Base: &commonpb.MsgBase{
			MsgType: commonpb.MsgType_WatchQueryChannels,
			MsgID:   rand.Int63(),
		},
		NodeID:       0,
		CollectionID: defaultCollectionID,
	}

	status, err := node.ReleaseCollection(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)

	node.UpdateStateCode(commonpb.StateCode_Abnormal)
	status, err = node.ReleaseCollection(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
}

func TestImpl_ReleasePartitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	req := &queryPb.ReleasePartitionsRequest{
		Base: &commonpb.MsgBase{
			MsgType: commonpb.MsgType_WatchQueryChannels,
			MsgID:   rand.Int63(),
		},
		NodeID:       0,
		CollectionID: defaultCollectionID,
		PartitionIDs: []UniqueID{defaultPartitionID},
	}

	status, err := node.ReleasePartitions(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)

	node.UpdateStateCode(commonpb.StateCode_Abnormal)
	status, err = node.ReleasePartitions(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_NotReadyServe, status.ErrorCode)
}

func TestImpl_GetSegmentInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("test GetSegmentInfo", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.GetSegmentInfoRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			SegmentIDs:   []UniqueID{},
			CollectionID: defaultCollectionID,
		}

		rsp, err := node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

		req.SegmentIDs = []UniqueID{-1}
		rsp, err = node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)
		assert.Equal(t, 0, len(rsp.GetInfos()))

		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		rsp, err = node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, rsp.Status.ErrorCode)
	})

	t.Run("test no collection in metaReplica", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		err = node.metaReplica.removeCollection(defaultCollectionID)
		assert.NoError(t, err)

		req := &queryPb.GetSegmentInfoRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			SegmentIDs:   []UniqueID{defaultSegmentID},
			CollectionID: defaultCollectionID,
		}

		rsp, err := node.GetSegmentInfo(ctx, req)
		assert.Nil(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)
	})

	t.Run("test different segment type", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.GetSegmentInfoRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			SegmentIDs:   []UniqueID{defaultSegmentID},
			CollectionID: defaultCollectionID,
		}

		seg, err := node.metaReplica.getSegmentByID(defaultSegmentID, segmentTypeSealed)
		assert.NoError(t, err)

		seg.setType(segmentTypeSealed)
		rsp, err := node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

		seg.setType(segmentTypeGrowing)
		rsp, err = node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

		seg.setType(-100)
		rsp, err = node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)
	})

	t.Run("test GetSegmentInfo with indexed segment", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		seg, err := node.metaReplica.getSegmentByID(defaultSegmentID, segmentTypeSealed)
		assert.NoError(t, err)

		seg.setIndexedFieldInfo(simpleFloatVecField.id, &IndexedFieldInfo{
			indexInfo: &queryPb.FieldIndexInfo{
				IndexName: "query-node-test",
				IndexID:   UniqueID(0),
				BuildID:   UniqueID(0),
			},
		})

		req := &queryPb.GetSegmentInfoRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			SegmentIDs:   []UniqueID{defaultSegmentID},
			CollectionID: defaultCollectionID,
		}

		rsp, err := node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.Status.ErrorCode)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		rsp, err = node.GetSegmentInfo(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, rsp.Status.ErrorCode)
	})
}

func TestImpl_isHealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := genSimpleQueryNode(ctx)
	assert.NoError(t, err)

	isHealthy := commonpbutil.IsHealthy(node.stateCode)
	assert.True(t, isHealthy)
}

func TestImpl_ShowConfigurations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	etcdCli, err := etcd.GetEtcdClient(
		Params.EtcdCfg.UseEmbedEtcd,
		Params.EtcdCfg.EtcdUseSSL,
		Params.EtcdCfg.Endpoints,
		Params.EtcdCfg.EtcdTLSCert,
		Params.EtcdCfg.EtcdTLSKey,
		Params.EtcdCfg.EtcdTLSCACert,
		Params.EtcdCfg.EtcdTLSMinVersion)
	assert.NoError(t, err)
	defer etcdCli.Close()

	t.Run("test ShowConfigurations", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)
		node.session = sessionutil.NewSession(node.queryNodeLoopCtx, Params.EtcdCfg.MetaRootPath, etcdCli)

		pattern := "Cache"
		req := &internalpb.ShowConfigurationsRequest{
			Base:    genCommonMsgBase(commonpb.MsgType_WatchQueryChannels, node.session.ServerID),
			Pattern: pattern,
		}

		resp, err := node.ShowConfigurations(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
	})

	t.Run("test ShowConfigurations node failed", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)
		node.session = sessionutil.NewSession(node.queryNodeLoopCtx, Params.EtcdCfg.MetaRootPath, etcdCli)
		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		pattern := "Cache"
		req := &internalpb.ShowConfigurationsRequest{
			Base:    genCommonMsgBase(commonpb.MsgType_WatchQueryChannels, node.session.ServerID),
			Pattern: pattern,
		}

		reqs, err := node.ShowConfigurations(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, reqs.Status.ErrorCode, commonpb.ErrorCode_NotReadyServe)
	})
}

func TestImpl_GetMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	etcdCli, err := etcd.GetEtcdClient(
		Params.EtcdCfg.UseEmbedEtcd,
		Params.EtcdCfg.EtcdUseSSL,
		Params.EtcdCfg.Endpoints,
		Params.EtcdCfg.EtcdTLSCert,
		Params.EtcdCfg.EtcdTLSKey,
		Params.EtcdCfg.EtcdTLSCACert,
		Params.EtcdCfg.EtcdTLSMinVersion)
	assert.NoError(t, err)
	defer etcdCli.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	t.Run("test GetMetrics", func(t *testing.T) {
		defer wg.Done()
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)
		node.session = sessionutil.NewSession(node.queryNodeLoopCtx, Params.EtcdCfg.MetaRootPath, etcdCli)

		metricReq := make(map[string]string)
		metricReq[metricsinfo.MetricTypeKey] = "system_info"
		mReq, err := json.Marshal(metricReq)
		assert.NoError(t, err)

		req := &milvuspb.GetMetricsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			Request: string(mReq),
		}

		_, err = node.GetMetrics(ctx, req)
		assert.NoError(t, err)
	})

	wg.Add(1)
	t.Run("test ParseMetricType failed", func(t *testing.T) {
		defer wg.Done()
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &milvuspb.GetMetricsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
		}

		_, err = node.GetMetrics(ctx, req)
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		_, err = node.GetMetrics(ctx, req)
		assert.NoError(t, err)
	})
	wg.Wait()
}

func TestImpl_ReleaseSegments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("test valid", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.ReleaseSegmentsRequest{
			Base:         genCommonMsgBase(commonpb.MsgType_ReleaseSegments, node.session.ServerID),
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			SegmentIDs:   []UniqueID{defaultSegmentID},
			Scope:        queryPb.DataScope_All,
		}

		_, err = node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)

		req.Scope = queryPb.DataScope_Streaming
		_, err = node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)

		req.Scope = queryPb.DataScope_Historical
		_, err = node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)
	})

	t.Run("test invalid query node", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.ReleaseSegmentsRequest{
			Base:         genCommonMsgBase(commonpb.MsgType_ReleaseSegments, node.session.ServerID),
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			SegmentIDs:   []UniqueID{defaultSegmentID},
		}

		node.UpdateStateCode(commonpb.StateCode_Abnormal)
		resp, err := node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.GetErrorCode())
	})

	t.Run("test target not matched", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.ReleaseSegmentsRequest{
			Base:         genCommonMsgBase(commonpb.MsgType_ReleaseSegments, -1),
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			SegmentIDs:   []UniqueID{defaultSegmentID},
		}

		resp, err := node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, resp.GetErrorCode())
	})

	t.Run("test segment not exists", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)

		req := &queryPb.ReleaseSegmentsRequest{
			Base:         genCommonMsgBase(commonpb.MsgType_ReleaseSegments, node.session.ServerID),
			CollectionID: defaultCollectionID,
			PartitionIDs: []UniqueID{defaultPartitionID},
			SegmentIDs:   []UniqueID{defaultSegmentID},
		}

		node.metaReplica.removeSegment(defaultSegmentID, segmentTypeSealed)

		status, err := node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	})

	t.Run("test no collection", func(t *testing.T) {
		node, err := genSimpleQueryNode(ctx)
		assert.NoError(t, err)
		err = node.metaReplica.removeCollection(defaultCollectionID)
		assert.NoError(t, err)

		req := &queryPb.ReleaseSegmentsRequest{
			Base:         genCommonMsgBase(commonpb.MsgType_ReleaseSegments, node.session.ServerID),
			CollectionID: defaultCollectionID,
		}

		status, err := node.ReleaseSegments(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	})
}

func TestImpl_Search(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	require.NoError(t, err)

	schema := genTestCollectionSchema()
	req, err := genSearchRequest(defaultNQ, IndexFaissIDMap, schema)
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)
	node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
	// shard cluster not synced
	_, err = node.Search(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)

	// shard cluster sync segments
	sc, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
	assert.True(t, ok)
	sc.SetupFirstVersion()

	_, err = node.Search(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)

	req.GetBase().TargetID = -1
	ret, err := node.Search(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, ret.GetStatus().GetErrorCode())
}

func TestImpl_SearchFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	require.NoError(t, err)

	schema := genTestCollectionSchema()
	req, err := genSearchRequest(defaultNQ, IndexFaissIDMap, schema)
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)
	node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
	sc, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
	assert.True(t, ok)
	sc.SetupFirstVersion()
	req.Base.TargetID = Params.QueryNodeCfg.GetNodeID()

	// collection not exist
	err = node.metaReplica.removeCollection(defaultCollectionID)
	assert.NoError(t, err)
	ret, err := node.Search(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, ret.GetStatus().GetErrorCode())

	// metric type mismatch
	col := node.metaReplica.addCollection(defaultCollectionID, schema)
	assert.NotNil(t, col)
	col.setMetricType("L2")
	req.MetricType = "IP"
	ret, err = node.Search(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, ret.GetStatus().GetErrorCode())

	t.Run("QueryNode not healthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		resp, err := node.Search(ctx, &queryPb.SearchRequest{
			Req:             req,
			FromShardLeader: false,
			DmlChannels:     []string{defaultDMLChannel},
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.Status.ErrorCode)
	})
}

func TestImpl_searchWithDmlChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	require.NoError(t, err)

	schema := genTestCollectionSchema()
	req, err := genSearchRequest(defaultNQ, IndexFaissIDMap, schema)
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)
	node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
	sc, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
	assert.True(t, ok)
	sc.SetupFirstVersion()

	_, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	}, defaultDMLChannel)
	assert.NoError(t, err)

	// test querynode plugin
	node.queryHook = &mockHook1{}
	newReq := typeutil.Clone(req)
	_, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             newReq,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
		TotalChannelNum: 1,
	}, defaultDMLChannel)
	assert.NoError(t, err)
	assert.Equal(t, req.SerializedExprPlan, newReq.SerializedExprPlan)

	node.queryHook = &mockHook2{}
	newReq = typeutil.Clone(req)
	_, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             newReq,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
		TotalChannelNum: 1,
	}, defaultDMLChannel)
	assert.NoError(t, err)
	assert.NotEqual(t, req.SerializedExprPlan, newReq.SerializedExprPlan)
	reqSearchParams, err := getSearchParamFromPlanExpr(req.SerializedExprPlan)
	assert.NoError(t, err)
	newReqSearchParams, err := getSearchParamFromPlanExpr(newReq.SerializedExprPlan)
	assert.NoError(t, err)
	assert.NotEqual(t, reqSearchParams, newReqSearchParams)
	assert.Equal(t, newReqSearchParams, "test")

	node.queryHook = &mockHook3{}
	newReq = typeutil.Clone(req)
	res, err := node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             newReq,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
		TotalChannelNum: 1,
	}, defaultDMLChannel)
	assert.NoError(t, err)
	assert.Equal(t, res.Status.Reason, fmt.Errorf("unexpected param").Error())

	node.queryHook = &mockHook3{}
	newReq = typeutil.Clone(req)
	newReq.SerializedExprPlan, _ = json.Marshal("")
	res, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             newReq,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
		TotalChannelNum: 1,
	}, defaultDMLChannel)
	assert.NoError(t, err)
	plan := &planpb.PlanNode{}
	err = proto.Unmarshal(newReq.SerializedExprPlan, plan)
	assert.Equal(t, res.Status.Reason, err.Error())
	node.queryHook = nil

	// search with ignore growing segment
	req.IgnoreGrowing = true
	_, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	}, defaultDMLChannel)
	assert.NoError(t, err)
	req.IgnoreGrowing = false

	// search for wrong dml channel
	_, err = node.searchWithDmlChannel(ctx, &queryPb.SearchRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel + "_suffix"},
	}, defaultDMLChannel)
	assert.NoError(t, err)
}

func TestImpl_GetCollectionStatistics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	defer node.Stop()
	require.NoError(t, err)

	req, err := genGetCollectionStatisticRequest()
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)

	_, err = node.GetStatistics(ctx, &queryPb.GetStatisticsRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)

	node.UpdateStateCode(commonpb.StateCode_Abnormal)
	resp, err := node.GetStatistics(ctx, &queryPb.GetStatisticsRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
	assert.NotEqual(t, resp.GetStatus().GetErrorCode(), commonpb.ErrorCode_Success)
}

func TestImpl_GetPartitionStatistics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	defer node.Stop()
	require.NoError(t, err)

	req, err := genGetPartitionStatisticRequest()
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)

	_, err = node.GetStatistics(ctx, &queryPb.GetStatisticsRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
}

func TestImpl_Query(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	defer node.Stop()
	require.NoError(t, err)

	schema := genTestCollectionSchema()
	req, err := genRetrieveRequest(schema)
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)
	node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
	// shard cluster not synced
	_, err = node.Query(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)

	// sync cluster segments
	sc, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
	assert.True(t, ok)
	sc.SetupFirstVersion()

	_, err = node.Query(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)

	req.GetBase().TargetID = -1
	ret, err := node.Query(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	})
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, ret.GetStatus().GetErrorCode())
}

func TestImpl_queryWithDmlChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := genSimpleQueryNode(ctx)
	defer node.Stop()
	require.NoError(t, err)

	schema := genTestCollectionSchema()
	req, err := genRetrieveRequest(schema)
	require.NoError(t, err)

	node.queryShardService.addQueryShard(defaultCollectionID, defaultDMLChannel, defaultReplicaID, 1)
	node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
	sc, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
	assert.True(t, ok)
	sc.SetupFirstVersion()

	_, err = node.queryWithDmlChannel(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	}, defaultDMLChannel)
	assert.NoError(t, err)

	// query with ignore growing
	req.IgnoreGrowing = true
	_, err = node.queryWithDmlChannel(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel},
	}, defaultDMLChannel)
	assert.NoError(t, err)
	req.IgnoreGrowing = false

	// query for wrong dml channel
	_, err = node.queryWithDmlChannel(ctx, &queryPb.QueryRequest{
		Req:             req,
		FromShardLeader: false,
		DmlChannels:     []string{defaultDMLChannel + "_suffix"},
	}, defaultDMLChannel)
	assert.NoError(t, err)

	t.Run("QueryNode not healthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		resp, err := node.Query(ctx, &queryPb.QueryRequest{
			Req:             req,
			FromShardLeader: false,
			DmlChannels:     []string{defaultDMLChannel},
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.Status.ErrorCode)
	})
}

func TestImpl_SyncReplicaSegments(t *testing.T) {
	t.Run("QueryNode not healthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		resp, err := node.SyncReplicaSegments(ctx, &querypb.SyncReplicaSegmentsRequest{})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.GetErrorCode())
	})

	t.Run("Sync non-exist channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		resp, err := node.SyncReplicaSegments(ctx, &querypb.SyncReplicaSegmentsRequest{
			VchannelName: defaultDMLChannel,
			ReplicaSegments: []*queryPb.ReplicaSegmentsInfo{
				{
					NodeId:      1,
					PartitionId: defaultPartitionID,
					SegmentIds:  []int64{1},
					Versions:    []int64{1},
				},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("Normal sync segments", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
		cs, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
		require.True(t, ok)
		cs.SetupFirstVersion()

		resp, err := node.SyncReplicaSegments(ctx, &querypb.SyncReplicaSegmentsRequest{
			VchannelName: defaultDMLChannel,
			ReplicaSegments: []*queryPb.ReplicaSegmentsInfo{
				{
					NodeId:      1,
					PartitionId: defaultPartitionID,
					SegmentIds:  []int64{1},
					Versions:    []int64{1},
				},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
		t.Log(resp.GetReason())

		segment, ok := cs.getSegment(1)
		require.True(t, ok)
		assert.Equal(t, common.InvalidNodeID, segment.nodeID)
		assert.Equal(t, defaultPartitionID, segment.partitionID)
		assert.Equal(t, segmentStateLoaded, segment.state)
	})
}

func TestSyncDistribution(t *testing.T) {
	t.Run("QueryNode not healthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		resp, err := node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.GetErrorCode())
	})

	t.Run("Target not match", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		resp, err := node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{
			Base:         &commonpb.MsgBase{TargetID: -1},
			CollectionID: defaultCollectionID,
			Channel:      defaultDMLChannel,
			Actions: []*querypb.SyncAction{
				{
					Type:        querypb.SyncType_Set,
					PartitionID: defaultPartitionID,
					SegmentID:   defaultSegmentID,
					NodeID:      99,
				},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, resp.GetErrorCode())
	})

	t.Run("Sync non-exist channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		resp, err := node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{
			Base:         &commonpb.MsgBase{TargetID: node.session.ServerID},
			CollectionID: defaultCollectionID,
			Channel:      defaultDMLChannel,
			Actions: []*querypb.SyncAction{
				{
					Type:        querypb.SyncType_Set,
					PartitionID: defaultPartitionID,
					SegmentID:   defaultSegmentID,
					NodeID:      99,
				},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("Normal sync segments", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
		cs, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
		require.True(t, ok)
		cs.SetupFirstVersion()

		resp, err := node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{
			Base:         &commonpb.MsgBase{TargetID: node.session.ServerID},
			CollectionID: defaultCollectionID,
			Channel:      defaultDMLChannel,
			Actions: []*querypb.SyncAction{
				{
					Type:        querypb.SyncType_Set,
					PartitionID: defaultPartitionID,
					SegmentID:   defaultSegmentID,
					NodeID:      99,
					Version:     1,
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())

		segment, ok := cs.getSegment(defaultSegmentID)
		require.True(t, ok)
		assert.Equal(t, common.InvalidNodeID, segment.nodeID)
		assert.Equal(t, defaultPartitionID, segment.partitionID)
		assert.Equal(t, segmentStateLoaded, segment.state)
		assert.EqualValues(t, 1, segment.version)
		resp, err = node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{
			Base:         &commonpb.MsgBase{TargetID: node.session.ServerID},
			CollectionID: defaultCollectionID,
			Channel:      defaultDMLChannel,
			Actions: []*querypb.SyncAction{
				{
					Type:        querypb.SyncType_Remove,
					PartitionID: defaultPartitionID,
					SegmentID:   defaultSegmentID,
					NodeID:      99,
					Version:     1,
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())

		cs, ok = node.ShardClusterService.getShardCluster(defaultDMLChannel)
		require.True(t, ok)
		_, ok = cs.getSegment(defaultSegmentID)
		require.False(t, ok)
	})

	t.Run("test unknown sync action type", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.ShardClusterService.addShardCluster(defaultCollectionID, defaultReplicaID, defaultDMLChannel, defaultVersion)
		cs, ok := node.ShardClusterService.getShardCluster(defaultDMLChannel)
		require.True(t, ok)
		cs.SetupFirstVersion()

		resp, err := node.SyncDistribution(ctx, &querypb.SyncDistributionRequest{
			Base:         &commonpb.MsgBase{TargetID: node.session.ServerID},
			CollectionID: defaultCollectionID,
			Channel:      defaultDMLChannel,
			Actions: []*querypb.SyncAction{
				{
					Type:        30,
					PartitionID: defaultPartitionID,
					SegmentID:   defaultSegmentID,
					NodeID:      99,
				},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})
}

func TestGetDataDistribution(t *testing.T) {
	t.Run("QueryNode not healthy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		node.UpdateStateCode(commonpb.StateCode_Abnormal)

		resp, err := node.GetDataDistribution(ctx, &querypb.GetDataDistributionRequest{})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NotReadyServe, resp.GetStatus().GetErrorCode())
	})

	t.Run("Target not match", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		node, err := genSimpleQueryNode(ctx)
		defer node.Stop()
		assert.NoError(t, err)

		resp, err := node.GetDataDistribution(ctx, &querypb.GetDataDistributionRequest{
			Base: &commonpb.MsgBase{TargetID: -1},
		})

		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_NodeIDNotMatch, resp.GetStatus().GetErrorCode())
	})
}

func TestIsUnavailableCode(t *testing.T) {
	node, err := genSimpleQueryNode(context.Background())
	defer node.Stop()
	assert.NoError(t, err)

	{
		failStatus := &commonpb.Status{}
		_, isUnavailable := isUnavailableCode(node, failStatus, failStatus, nil)
		assert.False(t, isUnavailable)
	}

	{
		failRet := &internalpb.GetStatisticsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
			},
		}
		_, isUnavailable := isUnavailableCode(node, failRet, failRet.Status, nil)
		assert.False(t, isUnavailable)
	}

	node.UpdateStateCode(commonpb.StateCode_Abnormal)

	{
		failStatus := &commonpb.Status{}
		failResp, isUnavailable := isUnavailableCode(node, failStatus, failStatus, nil)
		assert.True(t, isUnavailable)
		assert.Equal(t, failStatus, failResp)
	}

	{
		failRet := &internalpb.GetStatisticsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
			},
		}
		failResp, isUnavailable := isUnavailableCode(node, failRet, failRet.Status, func() {
			t.Log("logger is called")
		})
		assert.True(t, isUnavailable)
		assert.Equal(t, failRet, failResp)
	}
}