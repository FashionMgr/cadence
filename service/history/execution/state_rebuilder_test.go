// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package execution

import (
	ctx "context"
	"testing"
	"time"

	"github.com/uber/cadence/service/history/events"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/collection"
	"github.com/uber/cadence/common/definition"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/constants"
	"github.com/uber/cadence/service/history/shard"
)

type (
	stateRebuilderSuite struct {
		suite.Suite
		*require.Assertions

		controller          *gomock.Controller
		mockShard           *shard.TestContext
		mockEventsCache     *events.MockCache
		mockTaskRefresher   *MockMutableStateTaskRefresher
		mockDomainCache     *cache.MockDomainCache
		mockClusterMetadata *cluster.MockMetadata

		mockHistoryV2Mgr *mocks.HistoryV2Manager
		logger           log.Logger

		domainID   string
		workflowID string
		runID      string

		nDCStateRebuilder *stateRebuilderImpl
	}
)

func TestStateRebuilderSuite(t *testing.T) {
	s := new(stateRebuilderSuite)
	suite.Run(t, s)
}

func (s *stateRebuilderSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockTaskRefresher = NewMockMutableStateTaskRefresher(s.controller)

	s.mockShard = shard.NewTestContext(
		s.controller,
		&persistence.ShardInfo{
			ShardID:          10,
			RangeID:          1,
			TransferAckLevel: 0,
		},
		config.NewForTest(),
	)

	s.mockHistoryV2Mgr = s.mockShard.Resource.HistoryMgr
	s.mockDomainCache = s.mockShard.Resource.DomainCache
	s.mockClusterMetadata = s.mockShard.Resource.ClusterMetadata
	s.mockEventsCache = s.mockShard.MockEventsCache
	s.mockClusterMetadata.EXPECT().GetCurrentClusterName().Return(cluster.TestCurrentClusterName).AnyTimes()
	s.mockEventsCache.EXPECT().PutEvent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	s.logger = s.mockShard.GetLogger()

	s.workflowID = "some random workflow ID"
	s.runID = uuid.New()
	s.nDCStateRebuilder = NewStateRebuilder(
		s.mockShard, s.logger,
	).(*stateRebuilderImpl)
	s.nDCStateRebuilder.taskRefresher = s.mockTaskRefresher
}

func (s *stateRebuilderSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.Finish(s.T())
}

func (s *stateRebuilderSuite) TestInitializeBuilders() {
	mutableState, stateBuilder := s.nDCStateRebuilder.initializeBuilders(constants.TestGlobalDomainEntry)
	s.NotNil(mutableState)
	s.NotNil(stateBuilder)
	s.NotNil(mutableState.GetVersionHistories())
}

func (s *stateRebuilderSuite) TestApplyEvents() {

	requestID := uuid.New()
	events := []*shared.HistoryEvent{
		{
			EventId:                                 common.Int64Ptr(1),
			EventType:                               shared.EventTypeWorkflowExecutionStarted.Ptr(),
			WorkflowExecutionStartedEventAttributes: &shared.WorkflowExecutionStartedEventAttributes{},
		},
		{
			EventId:                                  common.Int64Ptr(2),
			EventType:                                shared.EventTypeWorkflowExecutionSignaled.Ptr(),
			WorkflowExecutionSignaledEventAttributes: &shared.WorkflowExecutionSignaledEventAttributes{},
		},
	}

	workflowIdentifier := definition.NewWorkflowIdentifier(s.domainID, s.workflowID, s.runID)

	mockStateBuilder := NewMockStateBuilder(s.controller)
	mockStateBuilder.EXPECT().ApplyEvents(
		s.domainID,
		requestID,
		shared.WorkflowExecution{
			WorkflowId: common.StringPtr(s.workflowID),
			RunId:      common.StringPtr(s.runID),
		},
		events,
		[]*shared.HistoryEvent(nil),
		true,
	).Return(nil, nil).Times(1)

	err := s.nDCStateRebuilder.applyEvents(workflowIdentifier, mockStateBuilder, events, requestID)
	s.NoError(err)
}

func (s *stateRebuilderSuite) TestPagination() {
	firstEventID := common.FirstEventID
	nextEventID := int64(101)
	branchToken := []byte("some random branch token")
	workflowIdentifier := definition.NewWorkflowIdentifier(s.domainID, s.workflowID, s.runID)

	event1 := &shared.HistoryEvent{
		EventId:                                 common.Int64Ptr(1),
		WorkflowExecutionStartedEventAttributes: &shared.WorkflowExecutionStartedEventAttributes{},
	}
	event2 := &shared.HistoryEvent{
		EventId:                              common.Int64Ptr(2),
		DecisionTaskScheduledEventAttributes: &shared.DecisionTaskScheduledEventAttributes{},
	}
	event3 := &shared.HistoryEvent{
		EventId:                            common.Int64Ptr(3),
		DecisionTaskStartedEventAttributes: &shared.DecisionTaskStartedEventAttributes{},
	}
	event4 := &shared.HistoryEvent{
		EventId:                              common.Int64Ptr(4),
		DecisionTaskCompletedEventAttributes: &shared.DecisionTaskCompletedEventAttributes{},
	}
	event5 := &shared.HistoryEvent{
		EventId:                              common.Int64Ptr(5),
		ActivityTaskScheduledEventAttributes: &shared.ActivityTaskScheduledEventAttributes{},
	}
	history1 := []*shared.History{{[]*shared.HistoryEvent{event1, event2, event3}}}
	history2 := []*shared.History{{[]*shared.HistoryEvent{event4, event5}}}
	history := append(history1, history2...)
	pageToken := []byte("some random token")

	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", &persistence.ReadHistoryBranchRequest{
		BranchToken:   branchToken,
		MinEventID:    firstEventID,
		MaxEventID:    nextEventID,
		PageSize:      NDCDefaultPageSize,
		NextPageToken: nil,
		ShardID:       common.IntPtr(s.mockShard.GetShardID()),
	}).Return(&persistence.ReadHistoryBranchByBatchResponse{
		History:       history1,
		NextPageToken: pageToken,
		Size:          12345,
	}, nil).Once()
	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", &persistence.ReadHistoryBranchRequest{
		BranchToken:   branchToken,
		MinEventID:    firstEventID,
		MaxEventID:    nextEventID,
		PageSize:      NDCDefaultPageSize,
		NextPageToken: pageToken,
		ShardID:       common.IntPtr(s.mockShard.GetShardID()),
	}).Return(&persistence.ReadHistoryBranchByBatchResponse{
		History:       history2,
		NextPageToken: nil,
		Size:          67890,
	}, nil).Once()

	paginationFn := s.nDCStateRebuilder.getPaginationFn(workflowIdentifier, firstEventID, nextEventID, branchToken)
	iter := collection.NewPagingIterator(paginationFn)

	result := []*shared.History{}
	for iter.HasNext() {
		item, err := iter.Next()
		s.NoError(err)
		result = append(result, item.(*shared.History))
	}

	s.Equal(history, result)
}

func (s *stateRebuilderSuite) TestRebuild() {
	requestID := uuid.New()
	version := int64(12)
	lastEventID := int64(2)
	branchToken := []byte("other random branch token")
	targetBranchToken := []byte("some other random branch token")
	now := time.Now()

	targetDomainID := uuid.New()
	targetDomainName := "other random domain name"
	targetWorkflowID := "other random workflow ID"
	targetRunID := uuid.New()

	firstEventID := common.FirstEventID
	nextEventID := lastEventID + 1
	events1 := []*shared.HistoryEvent{{
		EventId:   common.Int64Ptr(1),
		Version:   common.Int64Ptr(version),
		EventType: shared.EventTypeWorkflowExecutionStarted.Ptr(),
		WorkflowExecutionStartedEventAttributes: &shared.WorkflowExecutionStartedEventAttributes{
			WorkflowType:                        &shared.WorkflowType{Name: common.StringPtr("some random workflow type")},
			TaskList:                            &shared.TaskList{Name: common.StringPtr("some random workflow type")},
			Input:                               []byte("some random input"),
			ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(123),
			TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(233),
			Identity:                            common.StringPtr("some random identity"),
		},
	}}
	events2 := []*shared.HistoryEvent{{
		EventId:   common.Int64Ptr(2),
		Version:   common.Int64Ptr(version),
		EventType: shared.EventTypeWorkflowExecutionSignaled.Ptr(),
		WorkflowExecutionSignaledEventAttributes: &shared.WorkflowExecutionSignaledEventAttributes{
			SignalName: common.StringPtr("some random signal name"),
			Input:      []byte("some random signal input"),
			Identity:   common.StringPtr("some random identity"),
		},
	}}
	history1 := []*shared.History{{events1}}
	history2 := []*shared.History{{events2}}
	pageToken := []byte("some random pagination token")

	historySize1 := 12345
	historySize2 := 67890
	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", &persistence.ReadHistoryBranchRequest{
		BranchToken:   branchToken,
		MinEventID:    firstEventID,
		MaxEventID:    nextEventID,
		PageSize:      NDCDefaultPageSize,
		NextPageToken: nil,
		ShardID:       common.IntPtr(s.mockShard.GetShardID()),
	}).Return(&persistence.ReadHistoryBranchByBatchResponse{
		History:       history1,
		NextPageToken: pageToken,
		Size:          historySize1,
	}, nil).Once()
	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", &persistence.ReadHistoryBranchRequest{
		BranchToken:   branchToken,
		MinEventID:    firstEventID,
		MaxEventID:    nextEventID,
		PageSize:      NDCDefaultPageSize,
		NextPageToken: pageToken,
		ShardID:       common.IntPtr(s.mockShard.GetShardID()),
	}).Return(&persistence.ReadHistoryBranchByBatchResponse{
		History:       history2,
		NextPageToken: nil,
		Size:          historySize2,
	}, nil).Once()

	s.mockDomainCache.EXPECT().GetDomainByID(targetDomainID).Return(cache.NewGlobalDomainCacheEntryForTest(
		&persistence.DomainInfo{ID: targetDomainID, Name: targetDomainName},
		&persistence.DomainConfig{},
		&persistence.DomainReplicationConfig{
			ActiveClusterName: cluster.TestCurrentClusterName,
			Clusters: []*persistence.ClusterReplicationConfig{
				{ClusterName: cluster.TestCurrentClusterName},
				{ClusterName: cluster.TestAlternativeClusterName},
			},
		},
		1234,
		s.mockClusterMetadata,
	), nil).AnyTimes()
	s.mockTaskRefresher.EXPECT().RefreshTasks(now, gomock.Any()).Return(nil).Times(1)

	rebuildMutableState, rebuiltHistorySize, err := s.nDCStateRebuilder.Rebuild(
		ctx.Background(),
		now,
		definition.NewWorkflowIdentifier(s.domainID, s.workflowID, s.runID),
		branchToken,
		lastEventID,
		version,
		definition.NewWorkflowIdentifier(targetDomainID, targetWorkflowID, targetRunID),
		targetBranchToken,
		requestID,
	)
	s.NoError(err)
	s.NotNil(rebuildMutableState)
	rebuildExecutionInfo := rebuildMutableState.GetExecutionInfo()
	s.Equal(targetDomainID, rebuildExecutionInfo.DomainID)
	s.Equal(targetWorkflowID, rebuildExecutionInfo.WorkflowID)
	s.Equal(targetRunID, rebuildExecutionInfo.RunID)
	s.Equal(int64(historySize1+historySize2), rebuiltHistorySize)
	s.Equal(persistence.NewVersionHistories(
		persistence.NewVersionHistory(
			targetBranchToken,
			[]*persistence.VersionHistoryItem{persistence.NewVersionHistoryItem(lastEventID, version)},
		),
	), rebuildMutableState.GetVersionHistories())
	s.Equal(rebuildMutableState.GetExecutionInfo().StartTimestamp, now)
}