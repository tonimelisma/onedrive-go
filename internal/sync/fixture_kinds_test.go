package sync

func kindNames[K interface{ String() string }](kinds []K) []string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, kind.String())
	}

	return names
}
