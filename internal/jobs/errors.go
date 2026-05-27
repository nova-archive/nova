package jobs

import "errors"

var (
	// ErrNoJobsAvailable: Lease found no pending jobs. Callers should
	// poll again after a short sleep.
	ErrNoJobsAvailable = errors.New("jobs: no jobs available")

	// ErrJobNotFound: the job id does not match any row, or matches a
	// row in a terminal state.
	ErrJobNotFound = errors.New("jobs: job not found")

	// ErrUnknownKind: WorkerPool.Run found a job with no registered
	// handler. The job is failed with this error so operators can see
	// it in the admin UI and either register the handler or delete
	// the dead row.
	ErrUnknownKind = errors.New("jobs: unknown kind")
)
