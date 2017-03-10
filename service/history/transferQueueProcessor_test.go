package history

import (
	"math"
	"os"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	m "github.com/uber/cadence/.gen/go/matching"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber-common/bark"
)

type (
	transferQueueProcessorSuite struct {
		suite.Suite
		persistence.TestBase
		processor    *transferQueueProcessorImpl
		mockMatching *mocks.MatchingClient
		logger       bark.Logger
	}
)

func TestTransferQueueProcessorSuite(t *testing.T) {
	s := new(transferQueueProcessorSuite)
	suite.Run(t, s)
}

func (s *transferQueueProcessorSuite) SetupSuite() {
	if testing.Verbose() {
		log.SetOutput(os.Stdout)
	}

	logger := log.New()
	logger.Level = log.DebugLevel
	s.logger = bark.NewLoggerFromLogrus(logger)

	s.SetupWorkflowStore()
	s.mockMatching = &mocks.MatchingClient{}
	cache := newHistoryCache(s.ShardContext, s.logger)
	s.processor = newTransferQueueProcessor(s.ShardContext, s.mockMatching, cache).(*transferQueueProcessorImpl)
}

func (s *transferQueueProcessorSuite) TearDownSuite() {
	s.TearDownWorkflowStore()
}

func (s *transferQueueProcessorSuite) SetupTest() {
	// First cleanup transfer tasks from other tests and reset shard context
	s.ClearTransferQueue()
	s.processor.UpdateMaxAllowedReadLevel(math.MaxInt64)
}

func (s *transferQueueProcessorSuite) TestNoTransferTask() {
	tasksCh := make(chan *persistence.TransferTaskInfo)
	newPollInterval := s.processor.processTransferTasks(tasksCh, transferProcessorMinPollInterval)
	s.Equal(2*transferProcessorMinPollInterval, newPollInterval)
}

func (s *transferQueueProcessorSuite) TestSingleDecisionTask() {
	workflowExecution := workflow.WorkflowExecution{WorkflowId: common.StringPtr("single-decisiontask-test"),
		RunId:                                                    common.StringPtr("0d00698f-08e1-4d36-a3e2-3bf109f5d2d6")}
	taskList := "single-decisiontask-queue"
	task0, err0 := s.CreateWorkflowExecution(workflowExecution, taskList, []byte("decisiontask-scheduled"), nil, 3, 0, 2, nil)
	s.Nil(err0, "No error expected.")
	s.NotEmpty(task0, "Expected non empty task identifier.")

	tasksCh := make(chan *persistence.TransferTaskInfo, 10)
	newPollInterval := s.processor.processTransferTasks(tasksCh, time.Second)
	s.Equal(transferProcessorMinPollInterval, newPollInterval)
workerPump:
	for {
		select {
		case task := <-tasksCh:
			s.mockMatching.On("AddDecisionTask", mock.Anything, createAddRequestFromTask(task)).Once().Return(nil)
			s.processor.processTransferTask(task)
		default:
			break workerPump
		}
	}

	s.mockMatching.AssertExpectations(s.T())
}

func (s *transferQueueProcessorSuite) TestManyTransferTasks() {
	workflowExecution := workflow.WorkflowExecution{WorkflowId: common.StringPtr("many-transfertasks-test"),
		RunId:                                                    common.StringPtr("57d5f005-bdaa-42a5-a1c5-b9c45d8699a9")}
	taskList := "many-transfertasks-queue"
	activityTaskScheduleIds := []int64{2, 3, 4, 5, 6}
	task0, err0 := s.CreateWorkflowExecutionManyTasks(workflowExecution, taskList, "t1;t2;t3;t4;t5", nil, 7, 0, nil,
		activityTaskScheduleIds)
	s.Nil(err0, "No error expected.")
	s.NotEmpty(task0, "Expected non empty task identifier.")

	tasksCh := make(chan *persistence.TransferTaskInfo, 10)
	newPollInterval := s.processor.processTransferTasks(tasksCh, time.Second)
	s.Equal(transferProcessorMinPollInterval, newPollInterval)
workerPump:
	for {
		select {
		case task := <-tasksCh:
			s.mockMatching.On("AddActivityTask", mock.Anything, createAddRequestFromTask(task)).Once().Return(nil)
			s.processor.processTransferTask(task)
		default:
			break workerPump
		}
	}

	s.mockMatching.AssertExpectations(s.T())
}

func (s *transferQueueProcessorSuite) TestDeleteExecutionTransferTasks() {
	workflowID := "delete-execution-transfertasks-test"
	runID := "79fc8984-f78f-41cf-8fa1-4d383edb2cfd"
	workflowExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(workflowID),
		RunId:      common.StringPtr(runID),
	}
	taskList := "delete-execution-transfertasks-queue"
	identity := "delete-execution-transfertasks-test"
	builder := newHistoryBuilder(s.logger)
	addWorkflowExecutionStartedEvent(builder, workflowID, "wType", taskList, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, taskList, 100)
	history1, _ := builder.Serialize()
	task0, err0 := s.CreateWorkflowExecution(workflowExecution, taskList, history1, nil, 3, 0, 2, nil)
	s.Nil(err0, "No error expected.")
	s.NotEmpty(task0, "Expected non empty task identifier.")

	startedEvent := addDecisionTaskStartedEvent(builder, scheduleEvent.GetEventId(), taskList, identity)
	completeDecisionEvent := addDecisionTaskCompletedEvent(builder, scheduleEvent.GetEventId(), startedEvent.GetEventId(),
		nil, identity)
	addCompleteWorkflowEvent(builder, completeDecisionEvent.GetEventId(), []byte("result"))
	history2, _ := builder.Serialize()
	info1, _ := s.GetWorkflowExecutionInfo(workflowExecution)
	updatedInfo1 := copyWorkflowExecutionInfo(info1)
	updatedInfo1.History = history2
	updatedInfo1.NextEventID = int64(6)
	updatedInfo1.LastProcessedEvent = int64(2)
	err1 := s.UpdateWorkflowExecutionAndDelete(updatedInfo1, int64(3))
	s.Nil(err1, "No error expected.")

	newExecution := workflow.WorkflowExecution{WorkflowId: common.StringPtr("delete-execution-transfertasks-test"),
		RunId: common.StringPtr("d3ac892e-9fc1-4def-84fa-bfc44b9128cc")}
	_, err2 := s.CreateWorkflowExecution(newExecution, taskList, history1, nil, 3, 0, 2, nil)
	s.NotNil(err2, "Entity exist error expected.")
	s.logger.Infof("Error creating new execution: %v", err2)

	tasksCh := make(chan *persistence.TransferTaskInfo, 10)
	newPollInterval := s.processor.processTransferTasks(tasksCh, time.Second)
	s.Equal(transferProcessorMinPollInterval, newPollInterval)
workerPump:
	for {
		select {
		case task := <-tasksCh:
			if task.TaskType == persistence.TransferTaskTypeDecisionTask {
				s.mockMatching.On("AddDecisionTask", mock.Anything, createAddRequestFromTask(task)).Once().Return(nil)
			}
			s.processor.processTransferTask(task)
		default:
			break workerPump
		}
	}

	_, err3 := s.CreateWorkflowExecution(newExecution, taskList, history1, nil, 3, 0, 2, nil)
	s.Nil(err3, "No error expected.")
	s.logger.Infof("Execution created successfully: %v", err3)
	s.mockMatching.AssertExpectations(s.T())
}

func createAddRequestFromTask(task *persistence.TransferTaskInfo) interface{} {
	var res interface{}
	execution := workflow.WorkflowExecution{WorkflowId: common.StringPtr(task.WorkflowID),
		RunId:                                            common.StringPtr(task.RunID)}
	taskList := &workflow.TaskList{
		Name: &task.TaskList,
	}
	if task.TaskType == persistence.TransferTaskTypeActivityTask {
		res = &m.AddActivityTaskRequest{
			Execution:  &execution,
			TaskList:   taskList,
			ScheduleId: &task.ScheduleID,
		}
	} else if task.TaskType == persistence.TransferTaskTypeDecisionTask {
		res = &m.AddDecisionTaskRequest{
			Execution:  &execution,
			TaskList:   taskList,
			ScheduleId: &task.ScheduleID,
		}
	}
	return res
}

func containsID(list []int64, scheduleID int64) bool {
	for _, id := range list {
		if id == scheduleID {
			return true
		}
	}

	return false
}

func copyWorkflowExecutionInfo(sourceInfo *persistence.WorkflowExecutionInfo) *persistence.WorkflowExecutionInfo {
	return &persistence.WorkflowExecutionInfo{
		WorkflowID:           sourceInfo.WorkflowID,
		RunID:                sourceInfo.RunID,
		TaskList:             sourceInfo.TaskList,
		History:              sourceInfo.History,
		ExecutionContext:     sourceInfo.ExecutionContext,
		State:                sourceInfo.State,
		NextEventID:          sourceInfo.NextEventID,
		LastProcessedEvent:   sourceInfo.LastProcessedEvent,
		LastUpdatedTimestamp: sourceInfo.LastUpdatedTimestamp,
		DecisionPending:      sourceInfo.DecisionPending,
		CreateRequestID:      sourceInfo.CreateRequestID,
	}
}
