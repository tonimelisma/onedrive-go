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
		scopeFamily   string
		scopeAccess   string
		subjectKind   string
		subjectValue  string
		conditionType string
		timingSource  string
		blockedAtNano int64
		intervalNano  int64
		nextTrialNano int64
		trialCount    int
	)

	if err := scanner.Scan(
		&wireKey,
		&scopeFamily,
		&scopeAccess,
		&subjectKind,
		&subjectValue,
		&conditionType,
		&timingSource,
		&blockedAtNano,
		&intervalNano,
		&nextTrialNano,
		&trialCount,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	metadata, err := decodePersistedScopeMetadata(
		wireKey,
		scopeFamily,
		scopeAccess,
		subjectKind,
		subjectValue,
	)
	if err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}
	if metadata.Key.IsZero() {
		return &BlockScope{Key: ScopeKey{}}, nil
	}

	nextTrialAt := time.Time{}
	if nextTrialNano != 0 {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}

	return &BlockScope{
		Key:           metadata.Key,
		Family:        metadata.Family,
		Access:        metadata.Access,
		SubjectKind:   metadata.SubjectKind,
		SubjectValue:  metadata.SubjectValue,
		ConditionType: conditionType,
		TimingSource:  ScopeTimingSource(timingSource),
		BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
		TrialInterval: time.Duration(intervalNano),
		NextTrialAt:   nextTrialAt,
		TrialCount:    trialCount,
	}, nil
}
