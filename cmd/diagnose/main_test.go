package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/internal/diag"
)

func TestListNeedsNoModelCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data, _ := json.Marshal(diag.Event{Time: time.Now(), Kind: "http", Status: 500, Route: "/test", Message: "failure"})
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DIAG_LLM_URL", "")
	t.Setenv("DIAG_LLM_MODEL", "")
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"list", "--logs", path}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var scan diag.Scan
	if err := json.Unmarshal(out.Bytes(), &scan); err != nil || len(scan.Groups) != 1 {
		t.Fatalf("invalid list output: %s %v", out.String(), err)
	}
}

func TestMainTargetRejectedWithoutModelOrGit(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"run", "--base", "main", "--publish"}, &out, &stderr); err == nil || !strings.Contains(err.Error(), "custom/main") {
		t.Fatalf("main target was not rejected: %v", err)
	}
}

func TestHelpAndArgumentValidation(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"run", "--help"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "custom/main") {
		t.Fatal("help missing fixed target")
	}
	for _, args := range [][]string{{"unknown"}, {"list", "--window", "0s"}, {"list", "extra"}} {
		if err := run(context.Background(), args, &out, &stderr); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}
