package multisync

func finalDrainMountIDs(topology *childMountTopology) []string {
	if topology == nil {
		return nil
	}

	ids := make([]string, 0)
	records := sortedChildTopologyRecords(topology)
	for i := range records {
		record := &records[i]
		if record.state == childTopologyStatePendingRemoval &&
			record.stateReason == childTopologyStateReasonShortcutRemoved {
			ids = append(ids, record.mountID)
		}
	}

	return ids
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		values = append(values, value)
		seen[value] = struct{}{}
	}

	return values
}
