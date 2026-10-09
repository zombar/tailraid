// Command soak runs the scenario sequence of docs/09 §6 against the docker-compose
// cluster in test/soak and writes results.jsonl and report.html.
//
//	go run ./cmd/soak --profile quick --out report.html --results results.jsonl
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"time"

	"github.com/zombar/tailraid/internal/soak"
)

func main() {
	os.Exit(run())
}

func run() int {
	profile := flag.String("profile", "quick", "quick | full")
	out := flag.String("out", "report.html", "HTML report path")
	results := flag.String("results", "results.jsonl", "results stream path")
	dir := flag.String("compose-dir", "test/soak", "compose project directory")
	topology := flag.String("topology", "multi", "multi (4 hosts × 3 targets) | single (one host, four disks)")
	nodes := flag.Int("nodes", 0, "number of nodes in the compose file (default: 4 for multi, 1 for single)")
	seed := flag.Int64("seed", time.Now().UnixNano(), "workload seed")
	flag.Parse()

	var p soak.Profile
	switch *profile {
	case "quick":
		p = soak.Quick()
	case "full":
		p = soak.Quick()
		p.Name, p.BigFile, p.LargeFile, p.SmallFiles, p.ChurnOps, p.LeaderKills = "full", 64<<20, 1<<30, 2000, 3000, 10
	default:
		fmt.Fprintln(os.Stderr, "unknown profile", *profile)
		return 2
	}
	f, err := os.Create(*results)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	file, n := "docker-compose.yml", 4
	switch *topology {
	case "multi":
	case "single":
		file, n = "docker-compose.single.yml", 1
	default:
		fmt.Fprintln(os.Stderr, "unknown topology", *topology)
		return 2
	}
	if *nodes > 0 {
		n = *nodes
	}
	env := newComposeEnv(*dir, file, n)
	for _, n := range env.nodes { // every worker must answer before the run starts
		if err := env.waitReady(ctx, n.ref, 120*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	r := &soak.Run{Env: env, Profile: p, Log: log, Out: f, Rng: rand.New(rand.NewSource(*seed)), Seed: *seed} //nolint:gosec // workload
	log.Info("soak run", "profile", p.Name, "seed", *seed, "topology", *topology, "nodes", n)
	sum, err := r.Execute(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rf, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := soak.Render(rf, sum); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = rf.Close()
	for _, ph := range sum.Phases {
		state := "PASS"
		switch {
		case ph.Skipped:
			state = "SKIPPED (" + ph.Reason + ")"
		case ph.Failed > 0:
			state = fmt.Sprintf("FAIL (%d)", ph.Failed)
		}
		fmt.Printf("%-28s %s  %s\n", ph.Name, state, ph.Took.Round(time.Millisecond))
	}
	fmt.Printf("%d failed checks; report: %s\n", sum.Failed, *out)
	if sum.Failed > 0 {
		return 1
	}
	return 0
}
