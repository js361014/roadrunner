package pool

import (
	"context"

	"github.com/js361014/roadrunner/v2/payload"
	"github.com/js361014/roadrunner/v2/worker"
)

// Pool managed set of inner worker processes.
type Pool interface {
	// GetConfig returns pool configuration.
	GetConfig() interface{}

	// Exec executes task with payload
	Exec(rqs *payload.Payload) (*payload.Payload, error)

	// Workers returns worker list associated with the pool.
	Workers() (workers []worker.BaseProcess)

	// RemoveWorker removes worker from the pool.
	RemoveWorker(worker worker.BaseProcess) error

	// Reset kill all workers inside the watcher and replaces with new
	Reset(ctx context.Context) error

	// Destroy all underlying stack (but let them to complete the task).
	Destroy(ctx context.Context)

	// ExecWithContext executes task with context which is used with timeout
	execWithTTL(ctx context.Context, rqs *payload.Payload) (*payload.Payload, error)
}

// Watcher is an interface for the Sync workers lifecycle
type Watcher interface {
	// Watch used to add workers to the container
	Watch(workers []worker.BaseProcess) error

	// Take takes the first free worker
	Take(ctx context.Context) (worker.BaseProcess, error)

	// Release releases the worker putting it back to the queue
	Release(w worker.BaseProcess)

	// Allocate - allocates new worker and put it into the WorkerWatcher
	Allocate() error

	// Destroy destroys the underlying container
	Destroy(ctx context.Context)

	// Reset will replace container and workers array, kill all workers
	Reset(ctx context.Context)

	// List return all container w/o removing it from internal storage
	List() []worker.BaseProcess

	// Remove will remove worker from the container
	Remove(wb worker.BaseProcess)
}
