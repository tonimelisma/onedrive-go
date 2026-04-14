package sync

// PermissionCapability identifies the concrete access capability an action
// requires or a failed executor step attempted.
type PermissionCapability string

const (
	// The zero value intentionally means "unknown" so unset worker/test fields
	// still flow through the fallback capability inference paths.
	PermissionCapabilityUnknown     PermissionCapability = ""
	PermissionCapabilityLocalRead   PermissionCapability = "local_read"
	PermissionCapabilityLocalWrite  PermissionCapability = "local_write"
	PermissionCapabilityRemoteRead  PermissionCapability = "remote_read"
	PermissionCapabilityRemoteWrite PermissionCapability = "remote_write"
)

func (c PermissionCapability) IsLocal() bool {
	return c == PermissionCapabilityLocalRead || c == PermissionCapabilityLocalWrite
}

func (c PermissionCapability) IsRemote() bool {
	return c == PermissionCapabilityRemoteRead || c == PermissionCapabilityRemoteWrite
}

func issueTypeForPermissionCapability(capability PermissionCapability) string {
	switch capability {
	case PermissionCapabilityUnknown:
		return ""
	case PermissionCapabilityLocalRead:
		return IssueLocalReadDenied
	case PermissionCapabilityLocalWrite:
		return IssueLocalWriteDenied
	case PermissionCapabilityRemoteRead:
		return IssueRemoteReadDenied
	case PermissionCapabilityRemoteWrite:
		return IssueRemoteWriteDenied
	default:
		return ""
	}
}

func uniqueCapabilities(caps ...PermissionCapability) []PermissionCapability {
	if len(caps) == 0 {
		return nil
	}

	seen := make(map[PermissionCapability]struct{}, len(caps))
	out := make([]PermissionCapability, 0, len(caps))
	for _, cap := range caps {
		if cap == PermissionCapabilityUnknown {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}

	return out
}
