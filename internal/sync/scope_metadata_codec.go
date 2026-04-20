package sync

import "fmt"

type persistedScopeMetadata struct {
	Key          ScopeKey
	Descriptor   ScopeDescriptor
	Family       ScopeFamily
	Access       ScopeAccess
	SubjectKind  ScopeSubjectKind
	SubjectValue string
}

func encodePersistedScopeMetadata(key ScopeKey) (persistedScopeMetadata, error) {
	descriptor := DescribeScopeKey(key)
	if descriptor.IsZero() {
		return persistedScopeMetadata{}, fmt.Errorf("sync: unknown scope key %q", key.String())
	}

	return persistedScopeMetadata{
		Key:          key,
		Descriptor:   descriptor,
		Family:       descriptor.Family,
		Access:       descriptor.Access,
		SubjectKind:  descriptor.SubjectKind,
		SubjectValue: descriptor.SubjectValue,
	}, nil
}

func decodePersistedScopeMetadata(
	wireKey string,
	scopeFamily string,
	scopeAccess string,
	subjectKind string,
	subjectValue string,
) (persistedScopeMetadata, error) {
	key := ParseScopeKey(wireKey)
	if key.IsZero() {
		return persistedScopeMetadata{}, nil
	}

	metadata, err := encodePersistedScopeMetadata(key)
	if err != nil {
		return persistedScopeMetadata{}, fmt.Errorf("sync: decode scope metadata: %w", err)
	}

	if metadata.Family != ScopeFamily(scopeFamily) ||
		metadata.Access != ScopeAccess(scopeAccess) ||
		metadata.SubjectKind != ScopeSubjectKind(subjectKind) ||
		metadata.SubjectValue != subjectValue {
		return persistedScopeMetadata{}, fmt.Errorf(
			"sync: scope metadata mismatch for %q (family=%q access=%q subject_kind=%q subject_value=%q)",
			wireKey,
			scopeFamily,
			scopeAccess,
			subjectKind,
			subjectValue,
		)
	}

	return metadata, nil
}
