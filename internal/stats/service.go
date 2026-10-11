package stats

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// Sources are the node's own views the sampler reads (docs/17 §1). Health is the chunk
// walk; the sampler calls it on the leader at most once per ScanEvery and reuses it.
type Sources struct {
	Machine  func() *admin.MachineReport
	Gateway  func() *admin.GatewayReport
	Process  func() *admin.ProcessReport
	IsLeader func() bool
	Term     func() uint64
	Health   func() admin.VolumeHealth
	Stats    func() meta.Stats
	Members  func() []meta.Node
	Live     func(meta.NodeIdx) int
	Moved    func() (int64, bool) // the running pass's bytes moved
}

// ScanEvery bounds the leader's chunk walk for the cluster rows.
const ScanEvery = time.Minute

// Service samples, seals, rolls up and prunes: one per node.
type Service struct {
	Name   string
	Every  time.Duration
	Keep   Keep
	Clk    clock.Clock
	Src    Sources
	Writer *Writer
	Log    *slog.Logger

	mu       sync.Mutex
	prev     *Snapshot
	health   admin.VolumeHealth
	scanned  time.Time
	prevMove int64
	prevT    time.Time
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Sample takes one node row and, on the leader, one cluster row.
func (s *Service) Sample() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Clk.Now()
	cur := &Snapshot{T: now}
	if s.Src.Machine != nil {
		cur.Machine = s.Src.Machine()
	}
	if s.Src.Gateway != nil {
		cur.Gateway = s.Src.Gateway()
	}
	if s.Src.Process != nil {
		cur.Process = s.Src.Process()
	}
	s.Writer.Add(NodeRow(s.Name, s.prev, cur))
	s.prev = cur
	if s.Src.IsLeader == nil || !s.Src.IsLeader() {
		s.prevMove, s.prevT = -1, time.Time{}
		return
	}
	if s.Src.Health != nil && (s.scanned.IsZero() || now.Sub(s.scanned) >= ScanEvery) {
		s.health, s.scanned = s.Src.Health(), now
	}
	v := View{T: now, Health: s.health, Live: s.Src.Live, Moved: -1, PrevMove: s.prevMove, PrevT: s.prevT}
	if s.Src.Term != nil {
		v.Term = s.Src.Term()
	}
	v.By = s.Name
	if s.Src.Stats != nil {
		v.Stats = s.Src.Stats()
	}
	if s.Src.Members != nil {
		v.Members = s.Src.Members()
	}
	if s.Src.Moved != nil {
		if moved, ok := s.Src.Moved(); ok {
			v.Moved = moved
		}
	}
	s.Writer.Add(ClusterRow(v))
	s.prevMove, s.prevT = v.Moved, now
}

// Live is the RPC's answer: the unsealed rows newer than since (I-199).
func (s *Service) Live(since int64) []Row { return s.Writer.Live(since) }

// Seal writes the unsealed rows now (the hour boundary, a stop, `stats seal`) and rolls
// up any day still missing its rollup (docs/17 §7).
func (s *Service) Seal(ctx context.Context) error {
	now := s.Clk.Now()
	err := s.Writer.Seal(ctx, now)
	if cerr := s.Writer.CatchUp(ctx, now, s.Keep.Raw, s.writers()...); err == nil {
		err = cerr
	}
	return err
}

func (s *Service) writers() []string {
	if s.Src.IsLeader != nil && s.Src.IsLeader() {
		return []string{s.Name, ClusterWriter}
	}
	return []string{s.Name}
}

// Run samples every Every, seals at every UTC hour boundary, catches up the rollups in
// the first hour of a day, and returns once ctx ends (the caller seals on stop).
func (s *Service) Run(ctx context.Context) {
	if err := s.Writer.Recover(ctx); err != nil {
		s.log().Warn("stats: spilled segments not yet written", "err", err)
	}
	every := s.Every
	if every <= 0 {
		every = 10 * time.Second
	}
	sample := s.Clk.NewTimer(every)
	defer sample.Stop()
	nextHour := s.Clk.Now().Truncate(time.Hour).Add(time.Hour)
	hour := s.Clk.NewTimer(nextHour.Sub(s.Clk.Now()))
	defer hour.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sample.C():
			s.Sample()
			sample.Reset(every)
		case <-hour.C():
			if err := s.Writer.Seal(ctx, nextHour); err != nil {
				s.log().Warn("stats: seal failed", "err", err)
			}
			if err := s.Writer.CatchUp(ctx, nextHour, s.Keep.Raw, s.writers()...); err != nil {
				s.log().Warn("stats: rollup failed", "err", err)
			}
			nextHour = nextHour.Add(time.Hour)
			hour.Reset(max(nextHour.Sub(s.Clk.Now()), time.Second))
		}
	}
}

// Retain prunes the archive by Keep: the leader's hourly duty (I-194).
func (s *Service) Retain(ctx context.Context) (int, error) {
	return s.Writer.Retain(ctx, s.Clk.Now(), s.Keep)
}
