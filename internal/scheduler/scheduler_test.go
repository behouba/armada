package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-memdb"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/clock"

	protoutil "github.com/armadaproject/armada/internal/common/proto"
	"github.com/armadaproject/armada/internal/common/util"
	"github.com/armadaproject/armada/internal/scheduler/database"
	"github.com/armadaproject/armada/internal/scheduler/jobdb"
	"github.com/armadaproject/armada/internal/scheduler/schedulerobjects"
	"github.com/armadaproject/armada/pkg/armadaevents"
)

// Data to be used in tests
const maxLeaseReturns = 1

var (
	schedulingInfo      = &schedulerobjects.JobSchedulingInfo{AtMostOnce: true}
	schedulingInfoBytes = protoutil.MustMarshall(schedulingInfo)
)

var queuedJob = jobdb.NewJob(
	util.NewULID(),
	"testJobset",
	"testQueue",
	uint32(10),
	schedulingInfo,
	false,
	false,
	1)

var leasedJob = jobdb.NewJob(
	util.NewULID(),
	"testJobset",
	"testQueue",
	uint32(10),
	schedulingInfo,
	false,
	false,
	1).CreateRun("testExecutor")

// Test a single scheduler cycle
func TestScheduler_TestCycle(t *testing.T) {
	tests := map[string]struct {
		initialJobs          []*jobdb.SchedulerJob // jobs in the jobdb at the start of the cycle
		jobUpdates           []database.Job        // job updates from the database
		runUpdates           []database.Run        // run updates from the database
		staleExecutor        bool                  // if true then the executorRepository will report the executor as stale
		fetchError           bool                  // if true then the jobRepository will throw an error
		scheduleError        bool                  // if true then the schedulingalgo will throw an error
		publishError         bool                  // if true the publisher will throw an error
		expectedJobRunLeased []string              // ids of jobs we expect to have produced leased messages
		expectedJobRunErrors []string              // ids of jobs we expect to have produced jobRunErrors messages
		expectedJobErrors    []string              // ids of jobs we expect to have produced jobErrors messages
		expectedJobCancelled []string              // ids of jobs we expect to have  produced cancelled messages
		expectedJobSucceeded []string              // ids of jobs we expect to have  produced suceeeded messages
		expectedLeased       []string              // ids of jobs we expected to be leased in jobdb at the end of the cycle
		expectedQueued       []string              // ids of jobs we expected to be queued in jobdb at the end of the cycle
		expectedTerminal     []string              // ids of jobs we expected to be terminal in jobdb at the end of the cycle
	}{
		"Lease a single job already in the db": {
			initialJobs:          []*jobdb.SchedulerJob{queuedJob},
			expectedJobRunLeased: []string{queuedJob.GetId()},
			expectedLeased:       []string{queuedJob.GetId()},
		},
		"Lease a single job from an update": {
			jobUpdates: []database.Job{
				{
					JobID:  queuedJob.GetId(),
					JobSet: "testJobSet",
					Queue:  "testQueue",
					Serial: 1,
				},
			},
			expectedJobRunLeased: []string{queuedJob.GetId()},
			expectedLeased:       []string{queuedJob.GetId()},
		},
		"Nothing leased": {
			initialJobs:    []*jobdb.SchedulerJob{queuedJob},
			expectedQueued: []string{queuedJob.GetId()},
		},
		"No updates to an already leased job": {
			initialJobs:    []*jobdb.SchedulerJob{leasedJob},
			expectedLeased: []string{leasedJob.GetId()},
		},
		"Lease returned and re-queued": {
			initialJobs: []*jobdb.SchedulerJob{leasedJob},
			runUpdates: []database.Run{
				{
					RunID:    leasedJob.CurrentRun().GetId(),
					JobID:    leasedJob.GetId(),
					JobSet:   "testJobSet",
					Executor: "testExecutor",
					Failed:   true,
					Returned: true,
					Serial:   1,
				},
			},
			expectedQueued: []string{leasedJob.GetId()},
		},
		"Lease returned too many times": {
			initialJobs: []*jobdb.SchedulerJob{leasedJob},
			// 2 failures here so the second one should trigger a run failure
			runUpdates: []database.Run{
				{
					RunID:    leasedJob.CurrentRun().GetId(),
					JobID:    leasedJob.GetId(),
					JobSet:   "testJobSet",
					Executor: "testExecutor",
					Failed:   true,
					Returned: true,
					Serial:   1,
				},
				{
					RunID:    uuid.New(),
					JobID:    leasedJob.GetId(),
					JobSet:   "testJobSet",
					Executor: "testExecutor",
					Failed:   true,
					Returned: true,
					Serial:   2,
				},
			},
			expectedJobErrors: []string{leasedJob.GetId()},
			expectedTerminal:  []string{leasedJob.GetId()},
		},
		"Job cancelled": {
			initialJobs: []*jobdb.SchedulerJob{queuedJob},
			jobUpdates: []database.Job{
				{
					JobID:           queuedJob.GetId(),
					JobSet:          "testJobSet",
					Queue:           "testQueue",
					CancelRequested: true,
					Serial:          1,
				},
			},
			expectedJobCancelled: []string{queuedJob.GetId()},
			expectedTerminal:     []string{queuedJob.GetId()},
		},
		"Lease expired": {
			initialJobs:          []*jobdb.SchedulerJob{leasedJob},
			staleExecutor:        true,
			expectedJobRunErrors: []string{leasedJob.GetId()},
			expectedJobErrors:    []string{leasedJob.GetId()},
			expectedTerminal:     []string{leasedJob.GetId()},
		},
		"Job failed": {
			initialJobs: []*jobdb.SchedulerJob{leasedJob},
			runUpdates: []database.Run{
				{
					RunID:    leasedJob.CurrentRun().GetId(),
					JobID:    leasedJob.GetId(),
					JobSet:   "testJobSet",
					Executor: "testExecutor",
					Failed:   true,
					Serial:   1,
				},
			},
			expectedJobErrors: []string{leasedJob.GetId()},
			expectedTerminal:  []string{leasedJob.GetId()},
		},
		"Job succeeded": {
			initialJobs: []*jobdb.SchedulerJob{leasedJob},
			runUpdates: []database.Run{
				{
					RunID:     leasedJob.CurrentRun().GetId(),
					JobID:     leasedJob.GetId(),
					JobSet:    "testJobSet",
					Executor:  "testExecutor",
					Succeeded: true,
					Serial:    1,
				},
			},
			expectedJobSucceeded: []string{leasedJob.GetId()},
			expectedTerminal:     []string{leasedJob.GetId()},
		},
		"Fetch fails": {
			initialJobs:    []*jobdb.SchedulerJob{leasedJob},
			fetchError:     true,
			expectedLeased: []string{leasedJob.GetId()},
		},
		"Schedule fails": {
			initialJobs:    []*jobdb.SchedulerJob{leasedJob},
			scheduleError:  true,
			expectedLeased: []string{leasedJob.GetId()}, // job should still be leased as error was thrown and transaction rolled back
		},
		"Publish fails": {
			initialJobs:    []*jobdb.SchedulerJob{leasedJob},
			publishError:   true,
			expectedLeased: []string{leasedJob.GetId()}, // job should still be leased as error was thrown and transaction rolled back
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			clusterTimeout := 1 * time.Hour

			// Test objects
			jobRepo := &testJobRepository{
				updatedJobs: tc.jobUpdates,
				updatedRuns: tc.runUpdates,
				shouldError: tc.fetchError,
			}
			testClock := clock.NewFakeClock(time.Now())
			schedulingAlgo := &testSchedulingAlgo{jobsToSchedule: tc.expectedJobRunLeased, shouldError: tc.scheduleError}
			publisher := &testPublisher{shouldError: tc.publishError}
			stringInterner, err := util.NewStringInterner(100)
			require.NoError(t, err)

			heartbeatTime := testClock.Now()
			if tc.staleExecutor {
				heartbeatTime = heartbeatTime.Add(-2 * clusterTimeout)
			}
			clusterRepo := &testExecutorRepository{
				updateTimes: map[string]time.Time{"testExecutor": heartbeatTime},
			}
			sched, err := NewScheduler(
				jobRepo,
				clusterRepo,
				schedulingAlgo,
				NewStandaloneLeaderController(),
				publisher,
				stringInterner,
				1*time.Second,
				clusterTimeout,
				maxLeaseReturns)
			require.NoError(t, err)

			sched.clock = testClock

			// insert initial jobs
			txn := sched.jobDb.WriteTxn()
			err = sched.jobDb.Upsert(txn, tc.initialJobs)
			require.NoError(t, err)
			txn.Commit()

			// run a scheduler cycle
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = sched.cycle(ctx, false, sched.leaderController.GetToken())
			if tc.fetchError || tc.publishError || tc.scheduleError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			// assert that every event we've generated was expected
			outstandingLeaseMessages := stringSet(tc.expectedJobRunLeased)
			outstandingJobErrorMessages := stringSet(tc.expectedJobErrors)
			outstandingJobRunErrorMessages := stringSet(tc.expectedJobRunErrors)
			outstandingCancelledMessages := stringSet(tc.expectedJobCancelled)
			outstandingJobSucceededMessages := stringSet(tc.expectedJobSucceeded)
			for _, event := range publisher.events {
				for _, e := range event.Events {
					if e.GetJobRunLeased() != nil {
						leased := e.GetJobRunLeased()
						jobId, err := armadaevents.UlidStringFromProtoUuid(leased.JobId)
						require.NoError(t, err)
						_, ok := outstandingLeaseMessages[jobId]
						assert.True(t, ok)
						delete(outstandingLeaseMessages, jobId)
					} else if e.GetJobErrors() != nil {
						jobErrors := e.GetJobErrors()
						jobId, err := armadaevents.UlidStringFromProtoUuid(jobErrors.JobId)
						require.NoError(t, err)
						_, ok := outstandingJobErrorMessages[jobId]
						assert.True(t, ok)
						delete(outstandingJobErrorMessages, jobId)
					} else if e.GetJobRunErrors() != nil {
						jobErrors := e.GetJobRunErrors()
						jobId, err := armadaevents.UlidStringFromProtoUuid(jobErrors.JobId)
						require.NoError(t, err)
						_, ok := outstandingJobRunErrorMessages[jobId]
						assert.True(t, ok)
						delete(outstandingJobRunErrorMessages, jobId)
					} else if e.GetJobSucceeded() != nil {
						jobErrors := e.GetJobSucceeded()
						jobId, err := armadaevents.UlidStringFromProtoUuid(jobErrors.JobId)
						require.NoError(t, err)
						_, ok := outstandingJobSucceededMessages[jobId]
						assert.True(t, ok)
						delete(outstandingJobSucceededMessages, jobId)
					} else if e.GetCancelledJob() != nil {
						jobErrors := e.GetCancelledJob()
						jobId, err := armadaevents.UlidStringFromProtoUuid(jobErrors.JobId)
						require.NoError(t, err)
						_, ok := outstandingCancelledMessages[jobId]
						assert.True(t, ok)
						delete(outstandingCancelledMessages, jobId)
					} else {
						assert.Fail(t, fmt.Sprintf("unknown event sent to publisher %+v", e))
					}
				}
			}
			// Assert that we didn't miss out any events
			assert.Equal(t, 0, len(outstandingLeaseMessages))
			assert.Equal(t, 0, len(outstandingJobErrorMessages))
			assert.Equal(t, 0, len(outstandingJobRunErrorMessages))
			assert.Equal(t, 0, len(outstandingCancelledMessages))
			assert.Equal(t, 0, len(outstandingJobSucceededMessages))

			// assert that the serials are where we expect them to be
			if len(tc.jobUpdates) > 0 {
				assert.Equal(t, tc.jobUpdates[len(tc.jobUpdates)-1].Serial, sched.jobsSerial)
			} else {
				assert.Equal(t, int64(-1), sched.jobsSerial)
			}
			if len(tc.runUpdates) > 0 {
				assert.Equal(t, tc.runUpdates[len(tc.runUpdates)-1].Serial, sched.runsSerial)
			} else {
				assert.Equal(t, int64(-1), sched.runsSerial)
			}

			// assert that the job db is in the state we expect
			jobs, err := sched.jobDb.GetAll(sched.jobDb.ReadTxn())
			require.NoError(t, err)
			remainingLeased := stringSet(tc.expectedLeased)
			remainingQueued := stringSet(tc.expectedQueued)
			remainingTerminal := stringSet(tc.expectedTerminal)
			for _, job := range jobs {
				if job.InTerminalState() {
					_, ok := remainingTerminal[job.GetId()]
					assert.True(t, ok)
					delete(remainingTerminal, job.GetId())
				} else if job.GetQueued() {
					_, ok := remainingQueued[job.GetId()]
					assert.True(t, ok)
					delete(remainingQueued, job.GetId())
				} else {
					_, ok := remainingLeased[job.GetId()]
					assert.True(t, ok)
					delete(remainingLeased, job.GetId())
				}
			}
			assert.Equal(t, 0, len(remainingLeased))
			assert.Equal(t, 0, len(remainingQueued))
			assert.Equal(t, 0, len(remainingTerminal))
			cancel()
		})
	}
}

// Test running multiple scheduler cycles
func TestRun(t *testing.T) {
	// Test objects
	jobRepo := testJobRepository{numReceivedPartitions: 100}
	testClock := clock.NewFakeClock(time.Now())
	schedulingAlgo := &testSchedulingAlgo{}
	publisher := &testPublisher{}
	clusterRepo := &testExecutorRepository{}
	leaderController := NewStandaloneLeaderController()
	stringInterner, err := util.NewStringInterner(100)
	require.NoError(t, err)

	sched, err := NewScheduler(
		&jobRepo,
		clusterRepo,
		schedulingAlgo,
		leaderController,
		publisher,
		stringInterner,
		1*time.Second,
		1*time.Hour,
		maxLeaseReturns)
	require.NoError(t, err)

	sched.clock = testClock

	ctx, cancel := context.WithCancel(context.Background())

	//nolint:errcheck
	go sched.Run(ctx)

	time.Sleep(1 * time.Second)

	// Function that runs a cycle and waits until it sees published messages
	fireCycle := func() {
		publisher.Reset()
		wg := sync.WaitGroup{}
		wg.Add(1)
		sched.onCycleCompleted = func() { wg.Done() }
		jobId := util.NewULID()
		jobRepo.updatedJobs = []database.Job{{JobID: jobId, Queue: "testQueue"}}
		schedulingAlgo.jobsToSchedule = []string{jobId}
		testClock.Step(10 * time.Second)
		wg.Wait()
	}

	// fire a cycle and assert that we became leader and published
	fireCycle()
	assert.Equal(t, 1, len(publisher.events))

	// invalidate our leadership: we should not publish
	leaderController.token = InvalidLeaderToken()
	fireCycle()
	assert.Equal(t, 0, len(publisher.events))

	// become master again: we should publish
	leaderController.token = NewLeaderToken()
	fireCycle()
	assert.Equal(t, 1, len(publisher.events))

	cancel()
}

func TestScheduler_TestSyncState(t *testing.T) {
	tests := map[string]struct {
		initialJobs         []*jobdb.SchedulerJob // jobs in the jobdb at the start of the cycle
		jobUpdates          []database.Job        // job updates from the database
		runUpdates          []database.Run        // run updates from the database
		expectedUpdatedJobs []*jobdb.SchedulerJob
		expectedJobDbIds    []string
	}{
		"insert job": {
			jobUpdates: []database.Job{
				{
					JobID:          queuedJob.GetId(),
					JobSet:         queuedJob.GetJobset(),
					Queue:          queuedJob.GetQueue(),
					Submitted:      queuedJob.GetTimestamp(),
					Priority:       int64(queuedJob.GetPriority()),
					SchedulingInfo: schedulingInfoBytes,
					Serial:         1,
				},
			},
			expectedUpdatedJobs: []*jobdb.SchedulerJob{queuedJob},
			expectedJobDbIds:    []string{queuedJob.GetId()},
		},
		"insert job that already exists": {
			initialJobs: []*jobdb.SchedulerJob{queuedJob},
			jobUpdates: []database.Job{
				{
					JobID:          queuedJob.GetId(),
					JobSet:         queuedJob.GetJobset(),
					Queue:          queuedJob.GetQueue(),
					Submitted:      queuedJob.GetTimestamp(),
					Priority:       int64(queuedJob.GetPriority()),
					SchedulingInfo: schedulingInfoBytes,
					Serial:         1,
				},
			},
			expectedUpdatedJobs: []*jobdb.SchedulerJob{queuedJob},
			expectedJobDbIds:    []string{queuedJob.GetId()},
		},
		"add job run": {
			initialJobs: []*jobdb.SchedulerJob{queuedJob},
			runUpdates: []database.Run{
				{
					RunID:    uuid.UUID{},
					JobID:    queuedJob.GetId(),
					JobSet:   queuedJob.GetJobset(),
					Executor: "test-executor",
					Created:  123,
				},
			},
			expectedUpdatedJobs: []*jobdb.SchedulerJob{
				queuedJob.AddOrUpdateRun(
					jobdb.CreateRun(
						uuid.UUID{},
						123,
						"test-executor",
						false,
						false,
						false,
						false,
						false)).
					SetQueued(false),
			},
			expectedJobDbIds: []string{queuedJob.GetId()},
		},
		"job succeeded": {
			initialJobs: []*jobdb.SchedulerJob{queuedJob},
			jobUpdates: []database.Job{
				{
					JobID:          queuedJob.GetId(),
					JobSet:         queuedJob.GetJobset(),
					Queue:          queuedJob.GetQueue(),
					Submitted:      queuedJob.GetTimestamp(),
					Priority:       int64(queuedJob.GetPriority()),
					SchedulingInfo: schedulingInfoBytes,
					Succeeded:      true,
					Serial:         1,
				},
			},
			expectedUpdatedJobs: []*jobdb.SchedulerJob{},
			expectedJobDbIds:    []string{},
		},
		"lease returned": {
			initialJobs: []*jobdb.SchedulerJob{leasedJob},
			runUpdates: []database.Run{
				{
					JobID:    leasedJob.GetId(),
					JobSet:   leasedJob.GetId(),
					RunID:    leasedJob.CurrentRun().GetId(),
					Failed:   true,
					Returned: true,
					Created:  leasedJob.CurrentRun().GetCreated(),
				},
			},
			expectedUpdatedJobs: []*jobdb.SchedulerJob{
				leasedJob.
					AddOrUpdateRun(leasedJob.CurrentRun().SetReturned()).
					SetQueued(true),
			},
			expectedJobDbIds: []string{leasedJob.GetId()},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Test objects
			// Test objects
			jobRepo := &testJobRepository{
				updatedJobs: tc.jobUpdates,
				updatedRuns: tc.runUpdates,
			}
			schedulingAlgo := &testSchedulingAlgo{}
			publisher := &testPublisher{}
			clusterRepo := &testExecutorRepository{}
			leaderController := NewStandaloneLeaderController()
			stringInterner, err := util.NewStringInterner(100)
			require.NoError(t, err)

			sched, err := NewScheduler(
				jobRepo,
				clusterRepo,
				schedulingAlgo,
				leaderController,
				publisher,
				stringInterner,
				1*time.Second,
				1*time.Hour,
				maxLeaseReturns)
			require.NoError(t, err)

			// insert initial jobs
			txn := sched.jobDb.WriteTxn()
			err = sched.jobDb.Upsert(txn, tc.initialJobs)
			require.NoError(t, err)
			txn.Commit()

			updatedJobs, err := sched.syncState(ctx)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedUpdatedJobs, updatedJobs)
			allDbJobs, err := sched.jobDb.GetAll(sched.jobDb.ReadTxn())
			require.NoError(t, err)

			expectedIds := stringSet(tc.expectedJobDbIds)
			require.Equal(t, len(tc.expectedJobDbIds), len(allDbJobs))
			for _, job := range allDbJobs {
				_, ok := expectedIds[job.GetId()]
				assert.True(t, ok)
			}
		})
	}
}

// Test implementations of the interfaces needed by the Scheduler
type testJobRepository struct {
	updatedJobs           []database.Job
	updatedRuns           []database.Run
	errors                map[uuid.UUID]*armadaevents.JobRunErrors
	shouldError           bool
	numReceivedPartitions uint32
}

func (t *testJobRepository) FindInactiveRuns(ctx context.Context, runIds []uuid.UUID) ([]uuid.UUID, error) {
	// TODO implement me
	panic("implement me")
}

func (t *testJobRepository) FetchJobRunLeases(ctx context.Context, executor string, maxResults uint, excludedRunIds []uuid.UUID) ([]*database.JobRunLease, error) {
	// TODO implement me
	panic("implement me")
}

func (t *testJobRepository) FetchJobUpdates(ctx context.Context, jobSerial int64, jobRunSerial int64) ([]database.Job, []database.Run, error) {
	if t.shouldError {
		return nil, nil, errors.New("error fetchiung job updates")
	}
	return t.updatedJobs, t.updatedRuns, nil
}

func (t *testJobRepository) FetchJobRunErrors(ctx context.Context, runIds []uuid.UUID) (map[uuid.UUID]*armadaevents.JobRunErrors, error) {
	if t.shouldError {
		return nil, errors.New("error fetching job run errors")
	}
	return t.errors, nil
}

func (t *testJobRepository) CountReceivedPartitions(ctx context.Context, groupId uuid.UUID) (uint32, error) {
	if t.shouldError {
		return 0, errors.New("error counting received partitions")
	}
	return t.numReceivedPartitions, nil
}

type testExecutorRepository struct {
	updateTimes map[string]time.Time
	shouldError bool
}

func (t testExecutorRepository) GetExecutors(ctx context.Context) ([]*schedulerobjects.Executor, error) {
	panic("implement me")
}

func (t testExecutorRepository) GetLastUpdateTimes(ctx context.Context) (map[string]time.Time, error) {
	if t.shouldError {
		return nil, errors.New("error getting last update time")
	}
	return t.updateTimes, nil
}

func (t testExecutorRepository) StoreExecutor(ctx context.Context, executor *schedulerobjects.Executor) error {
	panic("implement me")
}

type testSchedulingAlgo struct {
	jobsToSchedule []string
	shouldError    bool
}

func (t *testSchedulingAlgo) Schedule(ctx context.Context, txn *memdb.Txn, jobDb *jobdb.JobDb) ([]*jobdb.SchedulerJob, error) {
	if t.shouldError {
		return nil, errors.New("error scheduling jobs")
	}
	jobs := make([]*jobdb.SchedulerJob, 0, len(t.jobsToSchedule))
	for _, id := range t.jobsToSchedule {
		job, _ := jobDb.GetById(txn, id)
		if job != nil {
			if !job.GetQueued() {
				return nil, errors.New(fmt.Sprintf("Was asked to lease %s but job was already leased", job.GetId()))
			}
			job = job.CreateRun("test-executor")
			jobs = append(jobs, job)
		} else {
			return nil, errors.New(fmt.Sprintf("Was asked to lease %s but job does not exist", job.GetId()))
		}
	}
	if len(jobs) > 0 {
		err := jobDb.Upsert(txn, jobs)
		if err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

type testPublisher struct {
	events      []*armadaevents.EventSequence
	shouldError bool
}

func (t *testPublisher) PublishMessages(ctx context.Context, events []*armadaevents.EventSequence, _ func() bool) error {
	t.events = events
	if t.shouldError {
		return errors.New("Error when publishing")
	}
	return nil
}

func (t *testPublisher) Reset() {
	t.events = nil
}

func (t *testPublisher) PublishMarkers(ctx context.Context, groupId uuid.UUID) (uint32, error) {
	return 100, nil
}

func stringSet(src []string) map[string]bool {
	set := make(map[string]bool, len(src))
	for _, s := range src {
		set[s] = true
	}
	return set
}
