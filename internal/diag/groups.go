package diag

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

type Group struct {
	Fingerprint string    `json:"fingerprint"`
	Count       int       `json:"count"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Sample      Event     `json:"sample"`
}

type Scan struct {
	Groups    []Group `json:"groups"`
	Malformed int     `json:"malformed_lines"`
	Overflow  int     `json:"overflow_events"`
}

// ScanLogs reads the rotated file first and tolerates a final partially written line.
// Memory, line length, file size, and fingerprint cardinality are bounded.
func ScanLogs(path string, since time.Time) (Scan, error) {
	result := Scan{Groups: []Group{}}
	groups := make(map[string]*Group)
	found := false
	for _, name := range []string{path + ".1", path} {
		f, err := os.Open(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		found = true
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return result, err
		}
		if !info.Mode().IsRegular() || info.Size() > 32<<20 {
			_ = f.Close()
			return result, fmt.Errorf("diagnostic input %s must be a regular file of at most 32 MiB", name)
		}
		reader := bufio.NewReader(io.LimitReader(f, info.Size()))
		for {
			line, err := reader.ReadSlice('\n')
			if err == bufio.ErrBufferFull {
				// Collect bounded JSONL records up to 64 KiB.
				buf := append([]byte(nil), line...)
				for err == bufio.ErrBufferFull {
					line, err = reader.ReadSlice('\n')
					if len(buf)+len(line) <= 64<<10 {
						buf = append(buf, line...)
					} else {
						buf = nil
						break
					}
				}
				if buf == nil {
					_ = f.Close()
					return result, fmt.Errorf("diagnostic line exceeds 64 KiB in %s", name)
				}
				line = buf
			}
			if err == io.EOF {
				break
			} // Writer may still be completing this record.
			if err != nil {
				_ = f.Close()
				return result, err
			}
			var e Event
			if json.Unmarshal(line, &e) != nil || e.Time.IsZero() || (e.Kind != "http" && e.Kind != "panic" && e.Kind != "upstream") || e.Status < 400 || e.Status > 599 {
				result.Malformed++
				continue
			}
			if e.Time.Before(since) || e.Time.After(time.Now().Add(5*time.Minute)) {
				continue
			}
			e = sanitizeEvent(e)
			fp := Fingerprint(e)
			g := groups[fp]
			if g == nil {
				if len(groups) >= 1000 {
					result.Overflow++
					continue
				}
				g = &Group{Fingerprint: fp, FirstSeen: e.Time, LastSeen: e.Time, Sample: e}
				groups[fp] = g
			}
			g.Count++
			if e.Time.Before(g.FirstSeen) {
				g.FirstSeen = e.Time
			}
			if !e.Time.Before(g.LastSeen) {
				g.LastSeen = e.Time
				g.Sample = e
			}
		}
		_ = f.Close()
	}
	if !found {
		return result, fmt.Errorf("no diagnostic log found at %s (enable CODEX_DIAG_ENABLED on the service)", path)
	}
	for _, g := range groups {
		result.Groups = append(result.Groups, *g)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		if result.Groups[i].Count != result.Groups[j].Count {
			return result.Groups[i].Count > result.Groups[j].Count
		}
		return result.Groups[i].Fingerprint < result.Groups[j].Fingerprint
	})
	return result, nil
}
