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
