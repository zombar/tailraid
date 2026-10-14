package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

var ctx = context.Background()

// memVolume is an in-memory stats.Volume.
type memVolume struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMem() *memVolume { return &memVolume{files: map[string][]byte{}} }

func (m *memVolume) WriteFile(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[name] = bytes.Clone(data)
	return nil
}

func (m *memVolume) ReadFile(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(b), nil
}

func (m *memVolume) List(_ context.Context, dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for p := range m.files {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func (m *memVolume) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, name)
	return nil
}

// names is a Names over fixed rows.
type names struct{}

func (names) User(idx meta.UserIdx) (meta.User, bool) {
	switch idx {
	case 1:
		return meta.User{Idx: 1, Name: "kim"}, true
	case 2:
		return meta.User{Idx: 2, Name: "lee"}, true
	}
	return meta.User{}, false
}

func (names) Group(idx meta.GroupIdx) (meta.Group, bool) {
	if idx == 3 {
		return meta.Group{Idx: 3, Name: "family"}, true
	}
	return meta.Group{}, false
}

func (names) PathOf(ino meta.Ino) ([]string, error) {
	if ino == 7 {
		return []string{"photos"}, nil
	}
	return nil, meta.ErrNotExist
}

func newWriter(vol *memVolume) (*Writer, *clock.Fake) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC))
	return &Writer{Vol: vol, Node: "pike", Clk: clk}, clk
}

// Every applied users and secrets operation maps to exactly one record with its kind,
// actor and target, a refused one to a refused record, and a command outside the log's
// scope to none.
// Invariant: I-357
func TestAudit_OneRecordPerOp(t *testing.T) {
	keyID := meta.UserKeyID{1, 2}
	for _, tc := range []struct {
		name   string
		cmd    meta.Command
		res    meta.Result
		want   Record
		absent bool
	}{
		{"invite", meta.Command{Op: meta.OpInvite, Actor: 1, InviteOp: &meta.InviteOp{Invite: meta.Invite{Name: "pat", Kind: meta.InviteUser, Admin: true}}}, meta.Result{}, Record{Actor: "kim", Kind: KindInvite, Target: "pat", Detail: "admin"}, false},
		{"bootstrap invite", meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{Name: "kim", Kind: meta.InviteUser, Admin: true}}}, meta.Result{}, Record{Kind: KindInvite, Target: "kim", Detail: "admin; bootstrap"}, false},
		{"key invite", meta.Command{Op: meta.OpInvite, Actor: 2, InviteOp: &meta.InviteOp{Invite: meta.Invite{Kind: meta.InviteKey, ForUser: 2}}}, meta.Result{}, Record{Actor: "lee", Kind: KindInvite, Target: "lee", Detail: "a key for another device"}, false},
		{"invite delete", meta.Command{Op: meta.OpInvite, Actor: 1, InviteOp: &meta.InviteOp{Delete: true, Invite: meta.Invite{Name: "pat"}}}, meta.Result{}, Record{Actor: "kim", Kind: KindInviteDelete, Target: "pat"}, false},
		{"user create", meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "lee"}}, meta.Result{Ino: 2}, Record{Actor: "lee", Kind: KindUserCreate, Target: "lee"}, false},
		{"key add", meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserKeyAdd}}, meta.Result{Ino: 2}, Record{Kind: KindUserKeyAdd, Target: "lee"}, false},
		{"key rm", meta.Command{Op: meta.OpUser, Actor: 1, UserOp: &meta.UserOp{Action: meta.UserKeyRemove, Target: 2, KeyID: keyID}}, meta.Result{}, Record{Actor: "kim", Kind: KindUserKeyRemove, Target: "lee", Key: "0102000000000000"}, false},
		{"disable", meta.Command{Op: meta.OpUser, Actor: 1, UserOp: &meta.UserOp{Action: meta.UserDisable, Target: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindUserDisable, Target: "lee"}, false},
		{"enable", meta.Command{Op: meta.OpUser, Actor: 1, UserOp: &meta.UserOp{Action: meta.UserEnable, Target: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindUserEnable, Target: "lee"}, false},
		{"rm", meta.Command{Op: meta.OpUser, Actor: 1, UserOp: &meta.UserOp{Action: meta.UserRemove, Target: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindUserRemove, Target: "lee"}, false},
		{"group create", meta.Command{Op: meta.OpGroup, Actor: 1, GroupOp: &meta.GroupOp{Action: meta.GroupCreate, Name: "family"}}, meta.Result{}, Record{Actor: "kim", Kind: KindGroupCreate, Target: "@family"}, false},
		{"group rm", meta.Command{Op: meta.OpGroup, Actor: 1, GroupOp: &meta.GroupOp{Action: meta.GroupRemove, Group: 3}}, meta.Result{}, Record{Actor: "kim", Kind: KindGroupRemove, Target: "@family"}, false},
		{"group add", meta.Command{Op: meta.OpGroup, Actor: 1, GroupOp: &meta.GroupOp{Action: meta.GroupAddMember, Group: 3, User: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindGroupAdd, Target: "@family", Detail: "lee"}, false},
		{"group remove", meta.Command{Op: meta.OpGroup, Actor: 1, GroupOp: &meta.GroupOp{Action: meta.GroupRemoveMember, Group: 3, User: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindGroupDrop, Target: "@family", Detail: "lee"}, false},
		{"grant", meta.Command{Op: meta.OpGrant, Actor: 1, GrantOp: &meta.GrantOp{Ino: 7, Subject: meta.Subject{Kind: meta.SubjectUser, Idx: 2}, Role: meta.RoleWrite}}, meta.Result{}, Record{Actor: "kim", Kind: KindGrant, Target: "/photos", Detail: "lee write"}, false},
		{"grant to a group", meta.Command{Op: meta.OpGrant, Actor: 1, GrantOp: &meta.GrantOp{Ino: 7, Subject: meta.Subject{Kind: meta.SubjectGroup, Idx: 3}, Role: meta.RoleRead}}, meta.Result{}, Record{Actor: "kim", Kind: KindGrant, Target: "/photos", Detail: "@family read"}, false},
		{"revoke", meta.Command{Op: meta.OpGrant, Actor: 1, GrantOp: &meta.GrantOp{Ino: 7, Subject: meta.Subject{Kind: meta.SubjectUser, Idx: 2}, Role: meta.RoleNone}}, meta.Result{}, Record{Actor: "kim", Kind: KindRevoke, Target: "/photos", Detail: "lee none"}, false},
		{"session mint", meta.Command{Op: meta.OpSession, Actor: 2, SessionOp: &meta.SessionOp{Key: keyID}}, meta.Result{}, Record{Actor: "lee", Kind: KindSessionMint, Key: "0102000000000000"}, false},
		{"session end", meta.Command{Op: meta.OpSession, Actor: 2, SessionOp: &meta.SessionOp{Delete: true}}, meta.Result{}, Record{Actor: "lee", Kind: KindSessionEnd}, false},
		{"session end all", meta.Command{Op: meta.OpSession, Actor: 1, SessionOp: &meta.SessionOp{Delete: true, All: true, User: 2}}, meta.Result{}, Record{Actor: "kim", Kind: KindSessionEnd, Detail: "every session of lee"}, false},
		{"secret set", meta.Command{Op: meta.OpSecretPut, Actor: 2, SecretPut: &meta.SecretPut{Secret: meta.Secret{Name: "user/lee/token", ByUser: 2}}}, meta.Result{}, Record{Actor: "lee", Kind: KindSecretSet, Target: "user/lee/token"}, false},
		{"secret rm", meta.Command{Op: meta.OpSecretDelete, Actor: 1, SecretDelete: &meta.SecretDelete{Name: "tmdb"}}, meta.Result{}, Record{Actor: "kim", Kind: KindSecretRemove, Target: "tmdb"}, false},
		{"secret rm all", meta.Command{Op: meta.OpSecretDelete, Actor: 1, SecretDelete: &meta.SecretDelete{All: true}}, meta.Result{}, Record{Actor: "kim", Kind: KindSecretRemove, Target: "*", Detail: "every secret and the key"}, false},
		{"refused", meta.Command{Op: meta.OpGroup, Actor: 2, GroupOp: &meta.GroupOp{Action: meta.GroupCreate, Name: "x"}}, meta.Result{Err: meta.ErrAccess}, Record{Actor: "lee", Kind: KindGroupCreate, Target: "@x", Result: ResultRefused, Detail: meta.ErrAccess.Error()}, false},
		{"a mkdir", meta.Command{Op: meta.OpMkdir, Actor: 2, Mkdir: &meta.Mkdir{Parent: 1, Name: "d"}}, meta.Result{}, Record{}, true},
		{"a key init", meta.Command{Op: meta.OpSecretKeyInit, SecretKeyInit: &meta.SecretKeyInit{}}, meta.Result{}, Record{}, true},
	} {
		got, ok := Applied(tc.cmd, 42, tc.res, names{})
		if ok == tc.absent {
			t.Errorf("%s: recorded %v, want %v", tc.name, ok, !tc.absent)
			continue
		}
		if !ok {
			continue
		}
		if tc.want.Result == "" {
			tc.want.Result = ResultOK
		}
		tc.want.Index = 42
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// Refusals by one actor of one kind within a minute are one record with their count,
// written when the minute closes; another actor or kind is another record; the count
// of the last hour serves status.
// Invariant: I-357
func TestAudit_RefusalRateLimit(t *testing.T) {
	vol := newMem()
	w, clk := newWriter(vol)
	for range 5 {
		w.Refuse("lee", "", KindMount, "/other/f", "open")
		clk.Advance(5 * time.Second)
	}
	w.Refuse("lee", "", KindWeb, "/api/ls", "")
	w.Refuse("kim", "", KindMount, "/x", "")
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(vol.files) != 0 {
		t.Fatalf("records written before the minute closed: %v", vol.files)
	}
	if n := w.RefusedLastHour(); n != 7 {
		t.Fatalf("refusals last hour %d, want 7", n)
	}
	clk.Advance(RefusalWindow)
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	recs, err := List(ctx, vol, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3: %+v", len(recs), recs)
	}
	counts := map[string]int{}
	for _, r := range recs {
		if r.Result != ResultRefused {
			t.Errorf("%+v: not refused", r)
		}
		counts[r.Actor+" "+r.Kind] = r.Count
	}
	if counts["lee file"] != 5 || counts["lee web"] != 1 || counts["kim file"] != 1 {
		t.Fatalf("counts %v", counts)
	}
	clk.Advance(time.Hour)
	if n := w.RefusedLastHour(); n != 0 {
		t.Fatalf("refusals last hour %d after an hour, want 0", n)
	}
}

// Each record carries the SHA-256 of the previous line: a segment decodes whole, a
// changed, dropped or inserted line breaks the chain from there, the records before it
// still readable; a segment survives the writer's restart with its chain intact.
// Invariant: I-358
func TestAudit_ChainHash(t *testing.T) {
	vol := newMem()
	w, clk := newWriter(vol)
	for i := range 4 {
		w.Record(Record{Actor: "kim", Kind: KindGrant, Target: "/p", Detail: string(rune('a' + i))})
		clk.Advance(time.Second)
	}
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	name := path.Join(Root, "pike", SegmentName(clk.Now()))
	data, err := vol.ReadFile(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := Decode(bytes.NewReader(data))
	if err != nil || len(recs) != 4 || recs[0].Prev != "" || recs[1].Prev == "" {
		t.Fatalf("decode: %v %d", err, len(recs))
	}
	lines := func() []string {
		gz, _ := gzip.NewReader(bytes.NewReader(data))
		b, _ := readAll(gz)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}()
	rezip := func(ls []string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write([]byte(strings.Join(ls, "\n") + "\n"))
		_ = gz.Close()
		return buf.Bytes()
	}
	for name, tc := range map[string]struct {
		lines  []string
		intact int
	}{
		"changed":  {[]string{lines[0], strings.Replace(lines[1], `"b"`, `"z"`, 1), lines[2], lines[3]}, 2}, // a changed line is caught by the next one's prev
		"dropped":  {[]string{lines[0], lines[2], lines[3]}, 1},
		"inserted": {[]string{lines[0], lines[1], lines[1], lines[2], lines[3]}, 2},
		"last":     {[]string{lines[0], lines[1], lines[2], strings.Replace(lines[3], `"d"`, `"q"`, 1)}, 4},
	} {
		got, err := Decode(bytes.NewReader(rezip(tc.lines)))
		if name == "last" { // the last line's own content is covered only by the next record: none here
			if err != nil || len(got) != 4 {
				t.Errorf("%s: %v %d", name, err, len(got))
			}
			continue
		}
		if !errors.Is(err, ErrChain) || len(got) != tc.intact {
			t.Errorf("%s: %v, %d intact, want ErrChain with %d", name, err, len(got), tc.intact)
		}
	}
	// a restart within the hour continues the chain
	w2, _ := newWriter(vol)
	w2.Clk = clk
	w2.Record(Record{Actor: "kim", Kind: KindGrant, Target: "/p", Detail: "e"})
	if err := w2.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	data, _ = vol.ReadFile(ctx, name)
	if recs, err := Decode(bytes.NewReader(data)); err != nil || len(recs) != 5 || recs[4].Detail != "e" {
		t.Fatalf("after a restart: %v %d", err, len(recs))
	}
	// the chain record in a listing of a tampered segment
	vol.files[name] = rezip([]string{lines[0], lines[2]})
	recs, err = List(ctx, vol, Filter{})
	if err != nil || len(recs) != 2 || recs[1].Kind != "audit.chain" || recs[0].Detail != "a" {
		t.Fatalf("a broken segment's listing: %v %+v", err, recs)
	}
}

func readAll(r *gzip.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

// The leader drops a segment whose hour ended more than the retention ago, on every
// node's directory, and keeps the seen file and the newer segments.
// Invariant: I-358
func TestAudit_Retention_Table(t *testing.T) {
	keep := 48 * time.Hour
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration // since the segment's hour ended
		gone bool
	}{
		{"now", 0, false}, {"one hour", time.Hour, false}, {"just inside", keep - time.Second, false}, {"at the edge", keep, false}, {"just past", keep + time.Second, true}, {"a week", 7 * 24 * time.Hour, true},
	} {
		vol := newMem()
		hour := now.Add(-tc.age).Add(-time.Hour)
		for _, n := range []string{"pike", "atlas"} {
			_ = vol.WriteFile(ctx, path.Join(Root, n, SegmentName(hour)), []byte("x"))
			_ = vol.WriteFile(ctx, path.Join(Root, n, SeenFile), []byte("{}"))
		}
		dropped, err := Retain(ctx, vol, keep, now)
		if err != nil {
			t.Fatal(err)
		}
		if (dropped == 2) != tc.gone || len(vol.files) != 4-dropped {
			t.Errorf("%s: dropped %d, %d files left, want gone %v", tc.name, dropped, len(vol.files), tc.gone)
		}
	}
}

// A listing merges every node's segments newest first and filters by time, user and
// kind (a kind ending in "." is a prefix); the limit keeps the newest.
// Invariant: I-358
func TestAudit_Ls_Filters(t *testing.T) {
	vol := newMem()
	w, clk := newWriter(vol)
	other, _ := newWriter(vol)
	other.Node, other.Clk = "atlas", clk
	w.Record(Record{Actor: "kim", Kind: KindGrant, Target: "/a"})
	clk.Advance(time.Hour)
	other.Record(Record{Actor: "lee", Kind: KindSecretSet, Target: "user/lee/x"})
	clk.Advance(time.Hour)
	w.Record(Record{Actor: "lee", Kind: KindSecretGet, Target: "user/lee/x"})
	clk.Advance(time.Hour)
	w.Record(Record{Actor: "kim", Kind: KindUserDisable, Target: "lee"})
	for _, x := range []*Writer{w, other} {
		if err := x.Flush(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		f    Filter
		want []string
	}{
		"all":      {Filter{}, []string{"user.disable", "secret.get", "secret.set", "grant"}},
		"user":     {Filter{User: "lee"}, []string{"secret.get", "secret.set"}},
		"kind":     {Filter{Kind: KindSecretGet}, []string{"secret.get"}},
		"prefix":   {Filter{Kind: "secret."}, []string{"secret.get", "secret.set"}},
		"since":    {Filter{Since: clk.Now().Add(-90 * time.Minute)}, []string{"user.disable", "secret.get"}},
		"limit":    {Filter{Limit: 1}, []string{"user.disable"}},
		"combined": {Filter{User: "kim", Kind: "grant"}, []string{"grant"}},
	} {
		recs, err := List(ctx, vol, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(recs))
		for i, r := range recs {
			got[i] = r.Kind
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	seen, err := Seen(ctx, vol)
	if err != nil || seen["kim"] != w.seen["kim"] || seen["lee"] <= seen["kim"]-3*int64(time.Hour) {
		t.Fatalf("seen %v %v", seen, err)
	}
	users := []meta.User{{Name: "kim", State: meta.UserActive, CreatedAt: 1}, {Name: "lee", State: meta.UserActive, CreatedAt: 1}, {Name: "pat", State: meta.UserActive, CreatedAt: 1}, {Name: "new", State: meta.UserActive, CreatedAt: clk.Now().UnixNano()}, {Name: "gone", State: meta.UserDisabled, CreatedAt: 1}}
	if d := Dormant(users, seen, 90*time.Minute, clk.Now()); strings.Join(d, " ") != "pat" {
		t.Fatalf("dormant %v, want pat (kim and lee seen within the window, new is young, gone is not active)", d)
	}
	if d := Dormant(users, seen, 30*time.Minute, clk.Now()); strings.Join(d, " ") != "lee pat" {
		t.Fatalf("dormant %v, want lee pat", d)
	}
}

// recordingVolume is a Volume whose segment write calls Record from another goroutine
// and waits for it, as the leader's apply hook does on raft's FSM goroutine while the
// flush's own write waits for its proposals to apply: a Record that waits for the flush
// is the deadlock of 2026-10-14 (make soak phase 15).
type recordingVolume struct {
	*memVolume
	w      *Writer
	during []Record // recorded from inside WriteFile, one per segment write
	calls  int
}

func (v *recordingVolume) WriteFile(ctx context.Context, name string, data []byte) error {
	if strings.HasSuffix(name, Suffix) && v.calls < len(v.during) {
		rec := v.during[v.calls]
		v.calls++
		done := make(chan struct{})
		go func() { v.w.Record(rec); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("Record waited for the segment write: %w", ctx.Err())
		}
	}
	return v.memVolume.WriteFile(ctx, name, data)
}

// Record never touches the volume and never waits for a flush: a record made while the
// writer's segment write is in progress returns at once and lands in the next flush,
// also one whose hour differs from the open segment's (the closed hour is written by
// the flush, never by Record).
// Invariant: I-333
func TestAudit_RecordNeverWaitsForTheVolume(t *testing.T) {
	vol := &recordingVolume{memVolume: newMem()}
	clk := clock.NewFake(time.Date(2026, 10, 14, 22, 20, 16, 0, time.UTC))
	w := &Writer{Vol: vol, Node: "pike", Clk: clk}
	vol.w = w
	nextHour := clk.Now().Add(time.Hour).UnixNano()
	vol.during = []Record{
		{Actor: "kim", Kind: KindSecretSet, Target: "s3/again/access_key"},
		{At: nextHour, Actor: "lee", Kind: KindGrant, Target: "/p"},
	}
	w.Record(Record{Actor: "kim", Kind: KindSecretSet, Target: "s3/first/access_key"})
	clk.Advance(time.Second)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// the first flush writes the segment; a record made during that write is in memory
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	// the second flush writes it, and the record made during this write opens the next hour
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	recs, err := List(ctx, vol, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, r := range recs {
		targets = append(targets, r.Target)
	}
	if want := []string{"/p", "s3/again/access_key", "s3/first/access_key"}; !slices.Equal(targets, want) {
		t.Fatalf("records %v, want %v", targets, want)
	}
	this := path.Join(Root, "pike", SegmentName(clk.Now()))
	next := path.Join(Root, "pike", SegmentName(clk.Now().Add(time.Hour)))
	if _, ok := vol.files[this]; !ok {
		t.Fatalf("this hour's segment missing: %v", vol.files)
	}
	if _, ok := vol.files[next]; !ok {
		t.Fatalf("the next hour's segment missing: %v", vol.files)
	}
}
