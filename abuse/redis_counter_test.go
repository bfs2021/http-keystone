package abuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func abuseTestContext(e *echo.Echo, remoteAddr, path string) *echo.Context {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	return e.NewContext(req, httptest.NewRecorder())
}

func TestAbuseWithRedisCounter(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	counter := store.NewRedisCounter(mr.Addr())
	defer func() { require.NoError(t, counter.Close()) }()
	require.NoError(t, counter.Ping(context.Background()))

	e := echo.New()
	handler := New(Config{
		Store:     counter,
		Threshold: 10,
		Cooldown:  time.Minute,
		Rules:     []Rule{{Path: "/evil", Score: 10}},
	})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	client := "192.0.2.1:1234"
	other := "198.51.100.7:4321"

	// The first /evil request reaches the threshold and is rejected.
	err = handler(abuseTestContext(e, client, "/evil"))
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusForbidden, he.Code)

	// The score is shared through Redis, so the same client is blocked on a
	// different path too.
	err = handler(abuseTestContext(e, client, "/safe"))
	require.Error(t, err)

	// Other clients are not affected.
	assert.NoError(t, handler(abuseTestContext(e, other, "/safe")))
	assert.Error(t, handler(abuseTestContext(e, other, "/evil")))

	// Resetting the shared counter unblocks the client.
	require.NoError(t, counter.Reset(context.Background(), "abuse:heat:192.0.2.1"))
	assert.NoError(t, handler(abuseTestContext(e, client, "/safe")))
}
