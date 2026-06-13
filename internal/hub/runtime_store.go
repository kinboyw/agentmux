package hub

import (
	"sync"
	"time"
)

type RuntimeStore interface {
	LoadWorkers() ([]workerRecord, error)
	SaveWorker(workerRecord) error
	DeleteWorker(workerID string) error
	DeleteTenantRuntime(tenantID string) error
	LoadUpdateJobs() ([]workerUpdateJob, error)
	SaveUpdateJob(workerUpdateJob) error
	AppendUpdateEvent(workerUpdateEvent) error
	ListUpdateEvents(jobID string) ([]workerUpdateEvent, error)
}

type memoryRuntimeStore struct {
	mu      sync.Mutex
	workers map[string]workerRecord
	jobs    map[string]workerUpdateJob
	events  map[string][]workerUpdateEvent
}

func newMemoryRuntimeStore() *memoryRuntimeStore {
	return &memoryRuntimeStore{
		workers: map[string]workerRecord{},
		jobs:    map[string]workerUpdateJob{},
		events:  map[string][]workerUpdateEvent{},
	}
}

func (s *memoryRuntimeStore) LoadWorkers() ([]workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workers := make([]workerRecord, 0, len(s.workers))
	for _, worker := range s.workers {
		worker.connected = false
		workers = append(workers, worker)
	}
	return workers, nil
}

func (s *memoryRuntimeStore) SaveWorker(worker workerRecord) error {
	if worker.id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers[worker.id] = worker
	return nil
}

func (s *memoryRuntimeStore) DeleteWorker(workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workers, workerID)
	for jobID, job := range s.jobs {
		if job.WorkerID == workerID {
			delete(s.jobs, jobID)
			delete(s.events, jobID)
		}
	}
	return nil
}

func (s *memoryRuntimeStore) DeleteTenantRuntime(tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for workerID, worker := range s.workers {
		if worker.tenantID == tenantID {
			delete(s.workers, workerID)
		}
	}
	for jobID, job := range s.jobs {
		if job.TenantID == tenantID {
			delete(s.jobs, jobID)
			delete(s.events, jobID)
		}
	}
	return nil
}

func (s *memoryRuntimeStore) LoadUpdateJobs() ([]workerUpdateJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]workerUpdateJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		job.Events = append([]workerUpdateEvent(nil), s.events[job.ID]...)
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *memoryRuntimeStore) SaveUpdateJob(job workerUpdateJob) error {
	if job.ID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := job
	copy.Events = nil
	s.jobs[job.ID] = copy
	return nil
}

func (s *memoryRuntimeStore) AppendUpdateEvent(event workerUpdateEvent) error {
	if event.ID == "" {
		event.ID = "evt_" + randomID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[event.JobID] = append(s.events[event.JobID], event)
	return nil
}

func (s *memoryRuntimeStore) ListUpdateEvents(jobID string) ([]workerUpdateEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]workerUpdateEvent(nil), s.events[jobID]...), nil
}
