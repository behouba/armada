package scheduler

import (
	"context"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"

	"github.com/armadaproject/armada/internal/armada/configuration"
)

type JobIterator interface {
	Next() (LegacySchedulerJob, error)
}

type JobRepository interface {
	GetQueueJobIds(queueName string) ([]string, error)
	GetExistingJobsByIds(ids []string) ([]LegacySchedulerJob, error)
}

type InMemoryJobIterator struct {
	i    int
	jobs []LegacySchedulerJob
}

func NewInMemoryJobIterator[S ~[]E, E LegacySchedulerJob](jobs S) *InMemoryJobIterator {
	vs := make([]LegacySchedulerJob, len(jobs))
	for i, job := range jobs {
		vs[i] = job
	}
	return &InMemoryJobIterator{
		jobs: vs,
	}
}

func (it *InMemoryJobIterator) Next() (LegacySchedulerJob, error) {
	if it.i >= len(it.jobs) {
		return nil, nil
	}
	v := it.jobs[it.i]
	it.i++
	return v, nil
}

type InMemoryJobRepository struct {
	jobsByQueue     map[string][]LegacySchedulerJob
	jobsById        map[string]LegacySchedulerJob
	priorityClasses map[string]configuration.PriorityClass
}

func NewInMemoryJobRepository(priorityClasses map[string]configuration.PriorityClass) *InMemoryJobRepository {
	return &InMemoryJobRepository{
		jobsByQueue:     make(map[string][]LegacySchedulerJob),
		jobsById:        make(map[string]LegacySchedulerJob),
		priorityClasses: maps.Clone(priorityClasses),
	}
}

func (repo *InMemoryJobRepository) EnqueueMany(jobs []LegacySchedulerJob) {
	updatedQueues := make(map[string]bool)
	for _, job := range jobs {
		queue := job.GetQueue()
		repo.jobsByQueue[queue] = append(repo.jobsByQueue[queue], job)
		repo.jobsById[job.GetId()] = job
		updatedQueues[queue] = true
	}
	for queue := range updatedQueues {
		repo.sortQueue(queue)
	}
}

func (repo *InMemoryJobRepository) Enqueue(job LegacySchedulerJob) {
	queue := job.GetQueue()
	repo.jobsByQueue[queue] = append(repo.jobsByQueue[queue], job)
	repo.jobsById[job.GetId()] = job
	repo.sortQueue(queue)
}

// Sort jobs queued jobs
// first by priority class priority, with higher values first,
// second by in-queue priority, with smaller values first, and
// finally by submit time, with earlier submit times first.
func (repo *InMemoryJobRepository) sortQueue(queue string) {
	slices.SortFunc(repo.jobsByQueue[queue], func(a, b LegacySchedulerJob) bool {
		infoa := a.GetRequirements(repo.priorityClasses)
		infob := b.GetRequirements(repo.priorityClasses)
		pca := repo.priorityClasses[infoa.PriorityClassName]
		pcb := repo.priorityClasses[infob.PriorityClassName]
		if pca.Priority > pcb.Priority {
			return true
		} else if pca.Priority < pcb.Priority {
			return false
		}
		if infoa.GetPriority() < infob.GetPriority() {
			return true
		} else if infoa.GetPriority() > infob.GetPriority() {
			return false
		}
		return infoa.GetSubmitTime().Before(infob.GetSubmitTime())
	})
}

func (repo *InMemoryJobRepository) GetQueueJobIds(queue string) ([]string, error) {
	jobs := repo.jobsByQueue[queue]
	rv := make([]string, len(jobs))
	for i, job := range jobs {
		rv[i] = job.GetId()
	}
	return rv, nil
}

func (repo *InMemoryJobRepository) GetExistingJobsByIds(jobIds []string) ([]LegacySchedulerJob, error) {
	rv := make([]LegacySchedulerJob, 0, len(jobIds))
	for _, jobId := range jobIds {
		if job, ok := repo.jobsById[jobId]; ok {
			rv = append(rv, job)
		}
	}
	return rv, nil
}

func (repo *InMemoryJobRepository) GetJobIterator(ctx context.Context, queue string) (JobIterator, error) {
	return NewInMemoryJobIterator(slices.Clone(repo.jobsByQueue[queue])), nil
}

// QueuedJobsIterator is an iterator over all jobs in a queue.
// It lazily loads jobs in batches from Redis asynch.
type QueuedJobsIterator struct {
	ctx context.Context
	err error
	c   chan LegacySchedulerJob
}

func NewQueuedJobsIterator(ctx context.Context, queue string, repo JobRepository) (*QueuedJobsIterator, error) {
	batchSize := 16
	g, ctx := errgroup.WithContext(ctx)
	it := &QueuedJobsIterator{
		ctx: ctx,
		c:   make(chan LegacySchedulerJob, 2*batchSize), // 2x batchSize to load one batch async.
	}

	jobIds, err := repo.GetQueueJobIds(queue)
	if err != nil {
		it.err = err
		return nil, err
	}
	g.Go(func() error { return queuedJobsIteratorLoader(ctx, jobIds, it.c, batchSize, repo) })

	return it, nil
}

func (it *QueuedJobsIterator) Next() (LegacySchedulerJob, error) {
	// Once this function has returned error,
	// it will return this error on every invocation.
	if it.err != nil {
		return nil, it.err
	}

	// Get one job that was loaded asynchrounsly.
	select {
	case <-it.ctx.Done():
		it.err = it.ctx.Err() // Return an error if called again.
		return nil, it.err
	case job, ok := <-it.c:
		if !ok {
			return nil, nil
		}
		return job, nil
	}
}

// queuedJobsIteratorLoader loads jobs from Redis lazily.
// Used with QueuedJobsIterator.
func queuedJobsIteratorLoader(ctx context.Context, jobIds []string, ch chan LegacySchedulerJob, batchSize int, repo JobRepository) error {
	defer close(ch)
	batch := make([]string, batchSize)
	for i, jobId := range jobIds {
		batch[i%len(batch)] = jobId
		if (i+1)%len(batch) == 0 || i == len(jobIds)-1 {
			jobs, err := repo.GetExistingJobsByIds(batch[:i%len(batch)+1])
			if err != nil {
				return err
			}
			for _, job := range jobs {
				if job == nil {
					continue
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ch <- job:
				}
			}
		}
	}
	return nil
}

// MultiJobsIterator chains several JobIterators together,
// emptying them in the order provided.
type MultiJobsIterator struct {
	i   int
	its []JobIterator
}

func NewMultiJobsIterator(its ...JobIterator) *MultiJobsIterator {
	return &MultiJobsIterator{
		its: its,
	}
}

func (it *MultiJobsIterator) Next() (LegacySchedulerJob, error) {
	if it.i >= len(it.its) {
		return nil, nil
	}
	if v, err := it.its[it.i].Next(); err != nil {
		return nil, err
	} else if v == nil {
		it.i++
		return it.Next()
	} else {
		return v, nil
	}
}
