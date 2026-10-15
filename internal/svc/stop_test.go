package svc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A stop plan is read from the manager's own state report: launchctl and sc.exe return
// before the process has exited, so only "not running" / STOPPED ends the wait; a query
// that fails names a service that is not loaded, so nothing runs (docs/12 §1).
// Invariant: I-131
func TestSvc_Stopped_Table(t *testing.T) {
	launchctl := []string{"launchctl", "print", "gui/501/app.tailraid.serve"}
	sc := []string{"sc.exe", "query", "tailraid"}
	cases := []struct {
		name  string
		query []string
		out   string
		err   error
		want  bool
	}{
		{"launchd running", launchctl, "gui/501/app.tailraid.serve = {\n\tstate = running\n\tpid = 80078\n", nil, false},
		{"launchd spawning", launchctl, "\tstate = xpcproxy\n\tpid = 80078\n", nil, false},
		{"launchd stopped", launchctl, "\tactive count = 0\n\tstate = not running\n", nil, true},
		{"launchd not loaded", launchctl, "Could not find service \"app.tailraid.serve\" in domain for port\n", errors.New("exit status 113"), true},
		{"scm running", sc, "SERVICE_NAME: tailraid\n        TYPE               : 10  WIN32_OWN_PROCESS\n        STATE              : 4  RUNNING\n", nil, false},
		{"scm stop pending", sc, "        STATE              : 3  STOP_PENDING\n", nil, false},
		{"scm stopped", sc, "        STATE              : 1  STOPPED\n", nil, true},
		{"scm not installed", sc, "[SC] EnumQueryServicesStatus:OpenService FAILED 1060:\n", errors.New("exit status 1060"), true},
	}
	for _, c := range cases {
		if got := stopped(c.query, c.out, c.err); got != c.want {
			t.Errorf("%s: stopped=%v, want %v", c.name, got, c.want)
		}
	}
}

// Stop on macOS and Windows waits until the manager reports no process, and a stop of a
// service that is not running succeeds; Linux's systemctl stop blocks on its own, so its
// plan has nothing to wait on. Apply is driven against a fake launchctl on PATH that
// reports the job running for two queries after the kill.
// Invariant: I-131
func TestSvc_StopWaitsForExit(t *testing.T) {
	p := Params{Exe: "/opt/tailraid", UID: 501}
	p.GOOS = "linux"
	pl, err := Stop(p)
	if err != nil || pl.WaitStopped != nil || strings.Join(pl.Commands[0], " ") != "sudo systemctl stop tailraid" {
		t.Fatalf("linux stop: %v %+v", err, pl)
	}
	p.GOOS = "windows"
	pl, _ = Stop(p)
	if strings.Join(pl.WaitStopped, " ") != "sc.exe query tailraid" || strings.Join(pl.Commands[0], " ") != "sc.exe stop tailraid" {
		t.Fatalf("windows stop: %+v", pl)
	}
	p.GOOS = "darwin"
	pl, _ = Stop(p)
	if strings.Join(pl.Commands[0], " ") != "launchctl kill SIGTERM gui/501/app.tailraid.serve" || strings.Join(pl.WaitStopped, " ") != "launchctl print gui/501/app.tailraid.serve" {
		t.Fatalf("darwin stop: %+v", pl)
	}
	var printed bytes.Buffer
	Print(&printed, pl)
	if !strings.Contains(printed.String(), "until the service has stopped") {
		t.Errorf("print does not show the wait:\n%s", printed.String())
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fake launchctl is a shell script")
	}

	// a fake launchctl: kill finds no process (exit 3, as launchctl does for a stopped
	// job) and print reports the job running until the third query
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$1" >> "` + calls + `"
case "$1" in
kill) echo "No process to signal."; exit 3 ;;
print)
  n=$(grep -c print "` + calls + `")
  if [ "$n" -lt 3 ]; then echo "	state = running"; else echo "	state = not running"; fi ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	if err := Apply(context.Background(), &out, pl); err != nil {
		t.Fatalf("stop of a job that exits after the signal: %v\n%s", err, out.String())
	}
	log, _ := os.ReadFile(calls)
	if got := strings.Count(string(log), "print"); got != 3 {
		t.Errorf("queried %d times, want 3 (two running, one stopped):\n%s", got, log)
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), "stopped") {
		t.Errorf("output does not end with stopped:\n%s", out.String())
	}

	// a job that is not loaded at all is an error: the service is not installed
	script = "#!/bin/sh\necho 'Could not find service \"app.tailraid.serve\" in domain for port'; exit 113\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), &out, pl); err == nil || !strings.Contains(err.Error(), "launchctl kill") {
		t.Fatalf("stop of an uninstalled service: %v", err)
	}
}
