package harvest

import "time"

type AccountCoverage struct {
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ReadySeconds   float64   `json:"ready_seconds"`
	MissingSeconds float64   `json:"missing_seconds"`
	Percent        *float64  `json:"percent"`
}
type CoverageSnapshot struct {
	StartedAt             *time.Time                `json:"started_at,omitempty"`
	UpdatedAt             *time.Time                `json:"updated_at,omitempty"`
	AccountCount          int                       `json:"account_count"`
	AverageReadySeconds   float64                   `json:"average_ready_seconds"`
	AverageMissingSeconds float64                   `json:"average_missing_seconds"`
	AveragePercent        *float64                  `json:"average_percent"`
	SampleIntervalSeconds int                       `json:"sample_interval_seconds"`
	Accounts              map[int64]AccountCoverage `json:"accounts"`
}
