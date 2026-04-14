package synctest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tbWithContext struct {
	testing.TB
	getCtx func() context.Context
}

func (tb tbWithContext) Context() context.Context {
	return tb.getCtx()
}

type testContextKey string

// Validates: R-6.10.10
func TestLoggerAndWriter(t *testing.T) {
	t.Parallel()

	logger := TestLogger(t)
	require.NotNil(t, logger)
	logger.Debug("hello from synctest")

	writer := newTestLogWriter(t)

	n, err := writer.Write([]byte("before-close"))
	require.NoError(t, err)
	assert.Equal(t, len("before-close"), n)

	writer.once.Do(func() { close(writer.done) })
	n, err = writer.Write([]byte("after-close"))
	require.NoError(t, err)
	assert.Equal(t, len("after-close"), n)
}

// Validates: R-6.10.10
func TestTestContextUsesBestAvailableContext(t *testing.T) {
	t.Parallel()

	customCtx := context.WithValue(context.Background(), testContextKey("k"), "v")
	assert.Same(t, customCtx, TestContext(tbWithContext{
		TB:     t,
		getCtx: func() context.Context { return customCtx },
	}))
	assert.Same(t, t.Context(), TestContext(t))
}
