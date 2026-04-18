package sync

const (
	wireLegacyThrottleAccount = "throttle:account"
	wireLegacyPermRemoteWrite = "perm:remote-write:"
)

func scopeKeyLikePattern(prefix string) string {
	return prefix + "%"
}

func permRemoteScopeKeyLikePattern() string {
	return scopeKeyLikePattern(WirePermRemote)
}

func legacyPermRemoteWriteLikePattern() string {
	return scopeKeyLikePattern(wireLegacyPermRemoteWrite)
}
