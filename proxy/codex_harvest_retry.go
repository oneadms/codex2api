package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func codexHarvestRetryAfter(raw string) time.Duration {
	if secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && secs > 0 {
		return time.Duration(min(secs, 86400)) * time.Second
	}
	if date, err := http.ParseTime(raw); err == nil && date.After(time.Now()) {
		return min(time.Until(date), 24*time.Hour)
	}
	return 0
}
