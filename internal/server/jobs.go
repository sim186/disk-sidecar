package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// Refused is an action refused for a reason worth showing to the user.
type Refused struct{ Msg string }

func (r *Refused) Error() string { return r.Msg }

func refuse(format string, a ...any) error { return &Refused{Msg: fmt.Sprintf(format, a...)} }

// Job is a long action running in the background; the page polls it for progress.
type Job struct {
	mu           sync.Mutex
	id           string
	state, phase string // state: running | done | error
	done, total  int64
	result       any
	err          string
}

func (j *Job) Update(phase string, done, total int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if phase != "" {
		j.phase = phase
	}
	if done >= 0 {
		j.done = done
	}
	if total >= 0 {
		j.total = total
	}
}

func (j *Job) SetDone(n int64) { j.mu.Lock(); j.done = n; j.mu.Unlock() }
func (j *Job) Advance(n int64) { j.mu.Lock(); j.done += n; j.mu.Unlock() }
func (j *Job) Phase(p string)  { j.mu.Lock(); j.phase = p; j.mu.Unlock() }

func (j *Job) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	var e any
	if j.err != "" {
		e = j.err
	}
	return map[string]any{"state": j.state, "phase": j.phase, "done": j.done, "total": j.total, "result": j.result, "error": e}
}

type jobTable struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	order []string
}

func newToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// start runs fn in the background. Only one action runs at a time.
func (s *Server) startJob(fn func(*Job) (any, error)) (*Job, error) {
	s.jt.mu.Lock()
	for _, j := range s.jt.jobs {
		j.mu.Lock()
		running := j.state == "running"
		j.mu.Unlock()
		if running {
			s.jt.mu.Unlock()
			return nil, refuse("un'altra operazione è ancora in corso: aspetta che finisca")
		}
	}
	job := &Job{id: newToken(8), state: "running", phase: "Preparazione…"}
	s.jt.jobs[job.id] = job
	s.jt.order = append(s.jt.order, job.id)
	for len(s.jt.order) > 8 { // keep the last few; the startup job stays
		old := s.jt.order[0]
		s.jt.order = s.jt.order[1:]
		if old != s.startupJob {
			delete(s.jt.jobs, old)
		}
	}
	s.jt.mu.Unlock()
	go func() {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		res, err := fn(job)
		job.mu.Lock()
		if err != nil {
			var r *Refused
			if errors.As(err, &r) {
				job.err = r.Msg
			} else {
				job.err = err.Error()
			}
			job.state = "error"
		} else {
			job.result, job.state = res, "done"
		}
		job.mu.Unlock()
		s.gen.Add(1)
	}()
	return job, nil
}

func (s *Server) job(id string) *Job {
	s.jt.mu.Lock()
	defer s.jt.mu.Unlock()
	return s.jt.jobs[id]
}
