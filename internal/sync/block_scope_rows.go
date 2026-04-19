package sync

import (
	"fmt"
	"time"
)

type blockScopeRowScanner interface {
	Scan(dest ...any) error
}

func scanBlockScopeRow(scanner blockScopeRowScanner) (*BlockScope, error) {
	var (
		wireKey       string
		issueType     string
		timingSource  string
		blockedAtNano int64
		intervalNano  int64
		nextTrialNano int64
		preserveNano  int64
		trialCount    int
	)

	if err := scanner.Scan(
		&wireKey,
		&issueType,
		&timingSource,
		&blockedAtNano,
		&intervalNano,
		&nextTrialNano,
		&preserveNano,
		&trialCount,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	nextTrialAt := time.Time{}
	if nextTrialNano != 0 {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}
	preserveUntil := time.Time{}
	if preserveNano != 0 {
		preserveUntil = time.Unix(0, preserveNano).UTC()
	}

	return &BlockScope{
		Key:           ParseScopeKey(wireKey),
		IssueType:     issueType,
		TimingSource:  ScopeTimingSource(timingSource),
		BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
		TrialInterval: time.Duration(intervalNano),
		NextTrialAt:   nextTrialAt,
		PreserveUntil: preserveUntil,
		TrialCount:    trialCount,
	}, nil
}
