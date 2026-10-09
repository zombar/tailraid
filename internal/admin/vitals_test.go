package admin_test

import (
	"crypto/sha256"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
)

// Every vital is a measurement the OS exposes or absent, never zero; the counters never
// go backwards.
// Invariant: I-85
func TestVitals_RealMeasurementsOrAbsent(t *testing.T) {
	first := admin.Vitals(t.TempDir())
	// burn some CPU so the process counter moves
	sum := sha256.Sum256([]byte("x"))
	for range 200_000 {
		sum = sha256.Sum256(sum[:])
	}
	m := admin.Vitals(t.TempDir())
	if m.CPUSeconds <= 0 || m.CPUSeconds < first.CPUSeconds {
		t.Fatalf("cpu seconds %v then %v", first.CPUSeconds, m.CPUSeconds)
	}
	if m.DiskTotal == 0 || m.DiskFree > m.DiskTotal {
		t.Fatalf("disk %d of %d", m.DiskFree, m.DiskTotal)
	}
	switch runtime.GOOS {
	case "linux":
		if m.RSS == 0 || m.RSSPeak == 0 || m.MemTotal == 0 || m.MemAvailable == 0 || m.MemAvailable > m.MemTotal || m.MachineTotal == 0 || m.MachineBusy > m.MachineTotal || m.MachineTotal < first.MachineTotal || m.DiskWritten < first.DiskWritten {
			t.Fatalf("linux vitals %+v", m)
		}
	case "darwin":
		if m.RSSPeak == 0 || m.MemTotal == 0 || m.MemAvailable == 0 || m.MemAvailable > m.MemTotal || m.Load1 < 0 || m.RSS != 0 || m.MachineTotal != 0 || m.DiskWritten != 0 {
			t.Fatalf("darwin vitals %+v", m)
		}
	case "windows":
		if m.MemTotal == 0 || m.MemAvailable > m.MemTotal || m.RSS != 0 || m.Load1 != 0 {
			t.Fatalf("windows vitals %+v", m)
		}
	}
	// absent, not zero: a field this OS cannot measure is not in the JSON
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" && strings.Contains(string(b), `"machine_total_ticks"`) {
		t.Fatalf("machine ticks reported off Linux: %s", b)
	}
	if strings.Contains(string(b), `:0,`) || strings.HasSuffix(string(b), `:0}`) {
		t.Fatalf("a zero vital was reported: %s", b)
	}
}
