package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/soak"
)

// The compose rig's half of soak phase 22 (docs/09 §6): the worker beside each node copies
// over the node's loopback SMB and NFS gateways; the faults are /dev calls and docker.

type copyStart struct {
	Proto string          `json:"proto"`
	Files []soak.CopySpec `json:"files"`
}

type copyState struct {
	ID      string            `json:"id"`
	Done    bool              `json:"done"`
	Results []soak.CopyResult `json:"results,omitempty"`
}

// composeCopyJob polls one worker's job.
type composeCopyJob struct {
	e  *composeEnv
	n  composeNode
	id string
}

func (j *composeCopyJob) Poll(ctx context.Context) (bool, []soak.CopyResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.n.worker+"/copy/"+j.id, http.NoBody)
	if err != nil {
		return false, nil, err
	}
	resp, err := j.e.http.Do(req)
	if err != nil {
		return false, nil, fmt.Errorf("%s worker: %w", j.n.ref.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil, fmt.Errorf("%s worker /copy/%s: %s", j.n.ref.Name, j.id, resp.Status)
	}
	var st copyState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return false, nil, err
	}
	return st.Done, st.Results, nil
}

func (e *composeEnv) StartCopies(ctx context.Context, ref soak.NodeRef, proto string, files []soak.CopySpec) (soak.CopyJob, error) {
	n := e.node(ref)
	var st copyState
	if err := e.post(ctx, n, "/copy", copyStart{Proto: proto, Files: files}, &st); err != nil {
		return nil, err
	}
	return &composeCopyJob{e: e, n: n, id: st.ID}, nil
}

// RestartGraceful stops the node with time to run its shutdown (flush, leadership
// handover; the plain Restart SIGKILLs after 2 s) and starts it, as an update does.
func (e *composeEnv) RestartGraceful(ctx context.Context, ref soak.NodeRef) error {
	if err := e.compose(ctx, "stop", "-t", "30", ref.Name); err != nil {
		return err
	}
	if err := e.compose(ctx, "start", ref.Name); err != nil {
		return err
	}
	if err := e.compose(ctx, "restart", "-t", "1", workerName(ref)); err != nil {
		return err
	}
	return e.waitReady(ctx, ref, 120*time.Second)
}

// RestartIdle is `update --restart`: admin.WaitIdle over the node's status, the same wait
// the CLI runs (I-221), then the graceful restart.
func (e *composeEnv) RestartIdle(ctx context.Context, ref soak.NodeRef) error {
	idle, err := admin.WaitIdle(ctx, clock.Real(), admin.DefaultIdleWait, func(ctx context.Context) (admin.Report, error) {
		return e.Status(ctx, ref)
	}, func(string) {})
	if err != nil {
		return err
	}
	if !idle {
		return fmt.Errorf("%s never went idle within %s", ref.Name, admin.DefaultIdleWait.Max)
	}
	return e.RestartGraceful(ctx, ref)
}

var _ soak.CopyEnv = (*composeEnv)(nil)

// SMBRaces runs phase 23's SMB handle races on the worker beside node (smbclient.Races).
func (e *composeEnv) SMBRaces(ctx context.Context, ref soak.NodeRef, dir string) ([]soak.RaceResult, error) {
	var out []soak.RaceResult
	if err := e.post(ctx, e.node(ref), "/smbraces", map[string]string{"dir": dir}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// HoldDeleteOnClose holds a delete-on-close handle open on the worker beside node until
// release (smbclient.Hold).
func (e *composeEnv) HoldDeleteOnClose(ctx context.Context, ref soak.NodeRef, path string) (func(context.Context) error, error) {
	n := e.node(ref)
	var held struct {
		ID string `json:"id"`
	}
	if err := e.post(ctx, n, "/smbhold", map[string]string{"path": path}, &held); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return e.post(ctx, n, "/smbrelease", map[string]string{"id": held.ID}, nil)
	}, nil
}
