package e2e

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	e2eRunAuthPreflightEnvVar        = "ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT"
	e2eRunFastFixturePreflightEnvVar = "ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT"
)

func verifierOwnedPreflightEnabled(envVar string) bool {
	return os.Getenv(envVar) == "1"
}

func requireVerifierOwnedPreflightEnv(t *testing.T, envVar string) {
	t.Helper()

	if verifierOwnedPreflightEnabled(envVar) {
		return
	}

	t.Skipf("skipping: verifier-owned preflight %s is disabled", envVar)
}

func TestVerifierOwnedPreflightEnabled(t *testing.T) {
	t.Setenv(e2eRunAuthPreflightEnvVar, "1")
	assert.True(t, verifierOwnedPreflightEnabled(e2eRunAuthPreflightEnvVar))
	requireVerifierOwnedPreflightEnv(t, e2eRunAuthPreflightEnvVar)

	t.Setenv(e2eRunAuthPreflightEnvVar, "0")
	assert.False(t, verifierOwnedPreflightEnabled(e2eRunAuthPreflightEnvVar))

	t.Setenv(e2eRunAuthPreflightEnvVar, "")
	assert.False(t, verifierOwnedPreflightEnabled(e2eRunAuthPreflightEnvVar))
}
