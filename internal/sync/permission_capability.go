package sync

// permissionCapability identifies the concrete access capability an action
// requires or a failed executor step attempted.
type permissionCapability string

const (
	// The zero value intentionally means "unknown" so unset worker/test fields
	// still flow through the fallback capability inference paths.
	permissionCapabilityUnknown     permissionCapability = ""
	permissionCapabilityLocalRead   permissionCapability = "local_read"
	permissionCapabilityLocalWrite  permissionCapability = "local_write"
	permissionCapabilityRemoteRead  permissionCapability = "remote_read"
	permissionCapabilityRemoteWrite permissionCapability = "remote_write"
)

func (c permissionCapability) IsLocal() bool {
	return c == permissionCapabilityLocalRead || c == permissionCapabilityLocalWrite
}

func (c permissionCapability) IsRemote() bool {
	return c == permissionCapabilityRemoteRead || c == permissionCapabilityRemoteWrite
}
