package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fired(ch <-chan time.Time) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestFake_NowAndAdvance(t *testing.T) {
	c := NewFake(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", c.Now(), t0)
	}
	c.Advance(90 * time.Second)
	if got := c.Since(t0); got != 90*time.Second {
		t.Fatalf("Since = %v", got)
	}
	c.Set(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Set did not move clock")
	}
}

func TestFake_AdvanceFiresExactlyDueTimers(t *testing.T) {
	c := NewFake(t0)
	a := c.NewTimer(10 * time.Second)
	b := c.NewTimer(20 * time.Second)
	c.Advance(10 * time.Second)
	if !fired(a.C()) {
		t.Fatal("timer a should have fired at exactly its deadline")
	}
	if fired(b.C()) {
		t.Fatal("timer b fired early")
	}
	c.Advance(9 * time.Second)
	if fired(b.C()) {
		t.Fatal("timer b fired early (19s)")
	}
	c.Advance(1 * time.Second)
	if !fired(b.C()) {
		t.Fatal("timer b should fire at 20s")
	}
	if fired(a.C()) {
		t.Fatal("timer a fired twice")
	}
}

func TestFake_TimersFireInDeadlineOrder(t *testing.T) {
	c := NewFake(t0)
	t3 := c.NewTimer(3 * time.Second) // seq 1
	t1 := c.NewTimer(1 * time.Second) // seq 2
	t2 := c.NewTimer(2 * time.Second) // seq 3
	c.Advance(5 * time.Second)
	for i, tm := range []Timer{t1, t2, t3} {
		if !fired(tm.C()) {
			t.Fatalf("timer %d did not fire", i+1)
		}
	}
	got := c.fireLog()
	want := []uint64{2, 3, 1}
	if len(got) != len(want) {
		t.Fatalf("fireLog = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fireLog = %v, want %v (deadline order)", got, want)
		}
	}
}

func TestFake_StopPreventsFire(t *testing.T) {
	c := NewFake(t0)
	tm := c.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on pending timer should return true")
	}
	if tm.Stop() {
		t.Fatal("second Stop should return false")
	}
	c.Advance(time.Hour)
	if fired(tm.C()) {
		t.Fatal("stopped timer fired")
	}
}

func TestFake_ResetReschedules(t *testing.T) {
	c := NewFake(t0)
	tm := c.NewTimer(time.Second)
	if !tm.Reset(10 * time.Second) {
		t.Fatal("Reset on pending timer should return true")
	}
	c.Advance(5 * time.Second)
	if fired(tm.C()) {
		t.Fatal("reset timer fired at old deadline")
	}
	c.Advance(5 * time.Second)
	if !fired(tm.C()) {
		t.Fatal("reset timer did not fire at new deadline")
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset on fired timer should return false")
	}
	c.Advance(time.Second)
	if !fired(tm.C()) {
		t.Fatal("re-armed timer did not fire")
	}
}

func TestFake_AfterAndZeroDuration(t *testing.T) {
	c := NewFake(t0)
	ch := c.After(0)
	if !fired(ch) {
		t.Fatal("After(0) should fire immediately")
	}
	ch2 := c.After(time.Minute)
	c.Advance(time.Minute)
	if !fired(ch2) {
		t.Fatal("After(1m) did not fire")
	}
}

func TestFake_TimerCreatedDuringAdvanceIsHonoured(t *testing.T) {
	c := NewFake(t0)
	outer := c.NewTimer(time.Second)
	var inner Timer
	done := make(chan struct{})
	go func() {
		<-outer.C()
		inner = c.NewTimer(time.Second) // deadline t0+2s, created while Advance(3s) runs
		close(done)
	}()
	c.Advance(time.Second)
	<-done
	c.Advance(2 * time.Second)
	if !fired(inner.C()) {
		t.Fatal("timer created after an advance must fire on the next advance past its deadline")
	}
}

func TestReal_Basics(t *testing.T) {
	c := Real()
	before := time.Now()
	if c.Now().Before(before) {
		t.Fatal("real clock went backwards")
	}
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	if tm.Stop() {
		t.Fatal("Stop after fire should be false")
	}
	<-c.After(time.Millisecond)
	if c.Since(before) <= 0 {
		t.Fatal("Since should be positive")
	}
}
