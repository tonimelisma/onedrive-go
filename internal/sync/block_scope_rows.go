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
		issueType     string
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
		&issueType,
		&timingSource,
		&blockedAtNano,
		&intervalNano,
		&nextTrialNano,
		&trialCount,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	key := ParseScopeKey(wireKey)
	if key.IsZero() {
		return &BlockScope{Key: ScopeKey{}}, nil
	}

	descriptor := DescribeScopeKey(key)
	if descriptor.Family == ScopeFamilyUnknown {
		return nil, fmt.Errorf("scan block scope row: unknown scope descriptor for %q", wireKey)
	}
	if descriptor.Family != ScopeFamily(scopeFamily) ||
		descriptor.Access != ScopeAccess(scopeAccess) ||
		descriptor.SubjectKind != ScopeSubjectKind(subjectKind) ||
		descriptor.SubjectValue != subjectValue {
		return nil, fmt.Errorf(
			"scan block scope row: metadata mismatch for %q (family=%q access=%q subject_kind=%q subject_value=%q)",
			wireKey,
			scopeFamily,
			scopeAccess,
			subjectKind,
			subjectValue,
		)
	}

	nextTrialAt := time.Time{}
	if nextTrialNano != 0 {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}

	return &BlockScope{
		Key:           key,
		Family:        descriptor.Family,
		Access:        descriptor.Access,
		SubjectKind:   descriptor.SubjectKind,
		SubjectValue:  descriptor.SubjectValue,
		IssueType:     issueType,
		TimingSource:  ScopeTimingSource(timingSource),
		BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
		TrialInterval: time.Duration(intervalNano),
		NextTrialAt:   nextTrialAt,
		TrialCount:    trialCount,
	}, nil
}
