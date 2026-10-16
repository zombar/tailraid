package webui

import (
	"context"
	"sync"
	"time"
)

// Thumbnails made in the background (docs/13 §5, I-374): a thumbnail the node makes reads
// the volume, seconds a file on a busy cluster, and the page loads four tiles at a time,
// so one being made held every tile behind it. Each is one job per key; a request waits
// for it at most ThumbWait and answers 202 after that while the job goes on.
const (
	// ThumbWait is the most a request waits for its thumbnail to be made.
	ThumbWait = 1500 * time.Millisecond
	// ThumbJobs is the most thumbnails made at once; past it a request starts nothing.
	ThumbJobs = 16
	// ThumbKeep is how long a finished job's answer waits for the request that retries.
	ThumbKeep = time.Minute
	// thumbMake bounds one job, run to its end whether anyone still waits or not.
	thumbMake = 2 * time.Minute
	// thumbKept bounds the finished answers kept.
	thumbKept = 256
)

// thumbJob is one thumbnail being made, or made within ThumbKeep.
type thumbJob struct {
	done chan struct{} // closed when made
	data []byte
	err  error
	at   time.Time // when made
}

// thumbJobs is the server's table of them.
type thumbJobs struct {
	mu      sync.Mutex
	jobs    map[string]*thumbJob
	running int
}

// later answers key's thumbnail as work makes it: a finished job's answer kept within ThumbKeep, or
// its job (started now, unless one runs or every place is taken) waited for at most
// ThumbWait. ready is false while the job runs or none could start: the caller answers
// 202. The request's context ends only the wait, never the making.
func (s *Server) later(ctx context.Context, key string, work func(context.Context) ([]byte, error)) (data []byte, err error, ready bool) {
	j := s.thumbJob(key, work)
	if j == nil {
		return nil, nil, false
	}
	select {
	case <-j.done:
		return j.data, j.err, true
	default:
	}
	t := s.o.Clk.NewTimer(ThumbWait)
	defer t.Stop()
	select {
	case <-j.done:
		return j.data, j.err, true
	case <-t.C():
	case <-ctx.Done():
	}
	return nil, nil, false
}

// thumbJob is key's job: a kept answer, the running job, or a new one when a place is free
// (nil when none is).
func (s *Server) thumbJob(key string, work func(context.Context) ([]byte, error)) *thumbJob {
	tj := &s.thumbs
	tj.mu.Lock()
	defer tj.mu.Unlock()
	now := s.o.Clk.Now()
	if tj.jobs == nil {
		tj.jobs = map[string]*thumbJob{}
	}
	if j, ok := tj.jobs[key]; ok {
		select {
		case <-j.done:
			if now.Sub(j.at) < ThumbKeep {
				return j
			}
			delete(tj.jobs, key) // an old answer: make it again
		default:
			return j
		}
	}
	if tj.running >= ThumbJobs {
		return nil
	}
	if len(tj.jobs) >= thumbKept { // finished answers past the bound go: a retry makes its own again
		for k, j := range tj.jobs {
			select {
			case <-j.done:
				delete(tj.jobs, k)
			default:
			}
		}
	}
	j := &thumbJob{done: make(chan struct{})}
	tj.jobs[key] = j
	tj.running++
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), thumbMake)
		defer cancel()
		data, err := work(ctx)
		tj.mu.Lock()
		j.data, j.err, j.at = data, err, s.o.Clk.Now()
		tj.running--
		tj.mu.Unlock()
		close(j.done)
	}()
	return j
}
