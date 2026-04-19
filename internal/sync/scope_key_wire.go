package sync

func scopeKeyLikePattern(prefix string) string {
	return prefix + "%"
}

func permRemoteScopeKeyLikePattern() string {
	return scopeKeyLikePattern(WirePermRemote)
}
