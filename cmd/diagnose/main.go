// diagnose reads structured service errors and prepares optional GitHub draft PRs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codex2api/internal/diag"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "diagnose:", diag.SafeText(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, `Usage: diagnose <list|run|watch> [flags]

list   Aggregate recent JSONL events without calling AI or GitHub.
run    Diagnose one batch and write local reports/patches.
watch  Repeat run at an interval with persistent deduplication.

Repairs always target custom/main. main is reserved for upstream updates.
Add --publish to push codex/autofix/* branches and open draft PRs.
No automatic merge or deployment. Build/tests run in GitHub CI after publication.
Model environment: DIAG_LLM_URL (full chat/completions URL), DIAG_LLM_MODEL,
DIAG_LLM_API_KEY. This command does not read the service's .env file.
Run 'diagnose run --help' for flags.`)
		return nil
	}
	command := args[0]
	if command != "list" && command != "run" && command != "watch" {
		return fmt.Errorf("unknown command %q", command)
	}
	c := diag.DefaultWorkerConfig()
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&c.LogPath, "logs", c.LogPath, "path to diagnostics.jsonl (also reads .1 rotation)")
	flags.DurationVar(&c.Window, "window", c.Window, "event lookback window")
	interval := 5 * time.Minute
	if command != "list" {
		flags.StringVar(&c.Repo, "repo", c.Repo, "local clone with a fetchable custom/main branch")
		flags.StringVar(&c.Remote, "remote", c.Remote, "Git remote for fetch and optional push")
		flags.StringVar(&c.Base, "base", c.Base, "repair target; only custom/main is allowed")
		flags.StringVar(&c.StateDir, "state-dir", c.StateDir, "private state and report directory")
		flags.IntVar(&c.MinCount, "min-count", c.MinCount, "minimum occurrences within the window")
		flags.IntVar(&c.MaxPerRun, "max-per-run", c.MaxPerRun, "maximum incidents per run (1-10)")
		flags.IntVar(&c.MaxAttempts, "max-attempts", c.MaxAttempts, "maximum attempts per fingerprint and base commit (1-10)")
		flags.DurationVar(&c.Cooldown, "cooldown", c.Cooldown, "delay before retrying failed incidents")
		flags.Float64Var(&c.MinConfidence, "min-confidence", c.MinConfidence, "minimum model confidence for preparing a patch")
		flags.BoolVar(&c.Publish, "publish", false, "push repair branches and create GitHub draft PRs targeting custom/main")
		if command == "watch" {
			flags.DurationVar(&interval, "interval", interval, "time between scans (at least 10s)")
		}
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if c.Window <= 0 {
		return errors.New("--window must be positive")
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if command == "list" {
		result, err := diag.ScanLogs(c.LogPath, time.Now().Add(-c.Window))
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	}
	if c.Base != diag.RepairBaseBranch {
		return errors.New("--base must be custom/main; main is reserved for upstream updates")
	}
	if interval < 10*time.Second {
		return errors.New("--interval must be at least 10s")
	}
	analyzer := &diag.ChatAnalyzer{URL: os.Getenv("DIAG_LLM_URL"), APIKey: os.Getenv("DIAG_LLM_API_KEY"), Model: os.Getenv("DIAG_LLM_MODEL")}
	if err := analyzer.Validate(); err != nil {
		return err
	}
	worker := &diag.Worker{Config: c, Analyzer: analyzer}
	for {
		result, err := worker.Run(ctx)
		if err == nil {
			if e := encoder.Encode(result); e != nil {
				return e
			}
			for _, outcome := range result.Outcomes {
				if outcome.Status == "failed" {
					err = errors.New("one or more incidents failed; see the result and state file")
					break
				}
			}
		}
		if command == "run" {
			return err
		}
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(stderr, "diagnose:", diag.SafeText(err.Error()))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
