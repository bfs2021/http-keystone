package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCacheContextWithHeaders builds a context whose request carries headers.
func newCacheContextWithHeaders(t *testing.T, e *echo.Echo, path string, headers map[string]string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// resolvedConfig mirrors the defaults New applies, so tests can compute the
// same cache keys the middleware uses.
func resolvedConfig(cfg Config) Config {
	if cfg.KeyGenerator == nil {
		cfg.KeyGenerator = DefaultKeyGenerator
	}
	if cfg.VaryHeaders == nil {
		cfg.VaryHeaders = []string{"Accept-Encoding"}
	}
	if cfg.CacheableStatusCodes == nil {
		cfg.CacheableStatusCodes = []int{http.StatusOK}
	}
	return cfg
}

func assertNotCached(t *testing.T, st store.Store, cfg Config, c *echo.Context) {
	t.Helper()
	_, err := st.Get(context.Background(), cacheKey(c, resolvedConfig(cfg)))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func assertCached(t *testing.T, st store.Store, cfg Config, c *echo.Context) *store.Entry {
	t.Helper()
	entry, err := st.Get(context.Background(), cacheKey(c, resolvedConfig(cfg)))
	require.NoError(t, err)
	return entry
}

func TestCacheBypassesAuthenticatedRequestsByDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"authorization", map[string]string{"Authorization": "Bearer secret"}},
		{"cookie", map[string]string{"Cookie": "session=abc"}},
		{"proxy-authorization", map[string]string{"Proxy-Authorization": "Basic abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			st := store.NewMemoryStore()
			calls := 0
			cfg := Config{Store: st, TTL: time.Minute}

			handler := New(cfg)(func(c *echo.Context) error {
				calls++
				return c.String(http.StatusOK, "private")
			})

			for i := 0; i < 2; i++ {
				c, rec := newCacheContextWithHeaders(t, e, "/private", tc.headers)
				require.NoError(t, handler(c))
				assert.Equal(t, "private", rec.Body.String())
				assert.Empty(t, rec.Header().Get("X-Cache"))
			}

			assert.Equal(t, 2, calls)
			c, _ := newCacheContextWithHeaders(t, e, "/private", tc.headers)
			assertNotCached(t, st, cfg, c)
		})
	}
}

func TestCacheAuthenticatedIsolatesUsers(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute, CacheAuthenticated: true}

	// The body depends on the credentials, so a shared key would leak data.
	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "user="+c.Request().Header.Get("Cookie"))
	})

	cA, recA := newCacheContextWithHeaders(t, e, "/me", map[string]string{"Cookie": "session=a"})
	require.NoError(t, handler(cA))
	assert.Equal(t, "user=session=a", recA.Body.String())
	assert.Equal(t, "MISS", recA.Header().Get("X-Cache"))

	cB, recB := newCacheContextWithHeaders(t, e, "/me", map[string]string{"Cookie": "session=b"})
	require.NoError(t, handler(cB))
	assert.Equal(t, "user=session=b", recB.Body.String())
	assert.Equal(t, "MISS", recB.Header().Get("X-Cache"))

	// Same user hits the cache and sees their own body.
	cA2, recA2 := newCacheContextWithHeaders(t, e, "/me", map[string]string{"Cookie": "session=a"})
	require.NoError(t, handler(cA2))
	assert.Equal(t, "user=session=a", recA2.Body.String())
	assert.Equal(t, "HIT", recA2.Header().Get("X-Cache"))

	// Without credentials the request is still a different entry.
	cAnon, recAnon := newCacheContextWithHeaders(t, e, "/me", nil)
	require.NoError(t, handler(cAnon))
	assert.Equal(t, "user=", recAnon.Body.String())
	assert.Equal(t, "MISS", recAnon.Header().Get("X-Cache"))

	assert.Equal(t, 3, calls)
}

func TestCacheAuthenticatedUsesAuthorizationInKey(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute, CacheAuthenticated: true}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "token="+c.Request().Header.Get("Authorization"))
	})

	headersA := map[string]string{"Authorization": "Bearer a"}
	headersB := map[string]string{"Authorization": "Bearer b"}

	cA, _ := newCacheContextWithHeaders(t, e, "/api", headersA)
	require.NoError(t, handler(cA))
	cB, recB := newCacheContextWithHeaders(t, e, "/api", headersB)
	require.NoError(t, handler(cB))
	assert.Equal(t, "token=Bearer b", recB.Body.String())

	cA2, recA2 := newCacheContextWithHeaders(t, e, "/api", headersA)
	require.NoError(t, handler(cA2))
	assert.Equal(t, "token=Bearer a", recA2.Body.String())
	assert.Equal(t, "HIT", recA2.Header().Get("X-Cache"))
	assert.Equal(t, 2, calls)
}

func TestCacheSkipsSetCookieResponses(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		c.Response().Header().Set("Set-Cookie", "session=generated")
		return c.String(http.StatusOK, "login")
	})

	first, rec1 := newCacheContext(t, e, http.MethodGet, "/login")
	require.NoError(t, handler(first))
	assert.Equal(t, "login", rec1.Body.String())
	assert.Equal(t, "session=generated", rec1.Header().Get("Set-Cookie"))
	assert.Equal(t, "BYPASS", rec1.Header().Get("X-Cache"))

	second, rec2 := newCacheContext(t, e, http.MethodGet, "/login")
	require.NoError(t, handler(second))
	assert.Equal(t, "session=generated", rec2.Header().Get("Set-Cookie"), "each client needs its own cookie")
	assert.Equal(t, 2, calls)
	assertNotCached(t, st, cfg, first)
}

func TestCacheSkipsPrivateResponseDirectives(t *testing.T) {
	for _, cacheControl := range []string{"private", "no-store", "no-cache", `private="Set-Cookie"`, "max-age=60, private"} {
		t.Run(cacheControl, func(t *testing.T) {
			e := echo.New()
			st := store.NewMemoryStore()
			calls := 0
			cfg := Config{Store: st, TTL: time.Minute}

			handler := New(cfg)(func(c *echo.Context) error {
				calls++
				c.Response().Header().Set("Cache-Control", cacheControl)
				return c.String(http.StatusOK, "sensitive")
			})

			for i := 0; i < 2; i++ {
				c, rec := newCacheContext(t, e, http.MethodGet, "/sensitive")
				require.NoError(t, handler(c))
				assert.Equal(t, "BYPASS", rec.Header().Get("X-Cache"))
			}

			assert.Equal(t, 2, calls)
		})
	}
}

func TestCacheSkipsUnknownVaryHeaders(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		c.Response().Header().Set("Vary", "Accept-Language")
		return c.String(http.StatusOK, `{"lang":"`+c.Request().Header.Get("Accept-Language")+`"}`)
	})

	for _, lang := range []string{"en", "fr"} {
		c, rec := newCacheContextWithHeaders(t, e, "/i18n", map[string]string{"Accept-Language": lang})
		require.NoError(t, handler(c))
		assert.Equal(t, "BYPASS", rec.Header().Get("X-Cache"))
	}
	assert.Equal(t, 2, calls)
}

func TestCacheVaryStarIsNeverCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		c.Response().Header().Set("Vary", "*")
		return c.String(http.StatusOK, "dynamic")
	})

	for i := 0; i < 2; i++ {
		c, rec := newCacheContext(t, e, http.MethodGet, "/dynamic")
		require.NoError(t, handler(c))
		assert.Equal(t, "BYPASS", rec.Header().Get("X-Cache"))
	}
	assert.Equal(t, 2, calls)
}

func TestCacheConfiguredVaryHeadersCreateVariants(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{
		Store:       st,
		TTL:         time.Minute,
		VaryHeaders: []string{"Accept-Encoding", "Accept-Language"},
	}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		c.Response().Header().Set("Vary", "Accept-Language")
		return c.String(http.StatusOK, "lang="+c.Request().Header.Get("Accept-Language"))
	})

	en, recEn := newCacheContextWithHeaders(t, e, "/i18n", map[string]string{"Accept-Language": "en"})
	require.NoError(t, handler(en))
	assert.Equal(t, "lang=en", recEn.Body.String())
	assert.Equal(t, "MISS", recEn.Header().Get("X-Cache"))

	fr, recFr := newCacheContextWithHeaders(t, e, "/i18n", map[string]string{"Accept-Language": "fr"})
	require.NoError(t, handler(fr))
	assert.Equal(t, "lang=fr", recFr.Body.String())
	assert.Equal(t, "MISS", recFr.Header().Get("X-Cache"))

	enAgain, recEnAgain := newCacheContextWithHeaders(t, e, "/i18n", map[string]string{"Accept-Language": "en"})
	require.NoError(t, handler(enAgain))
	assert.Equal(t, "lang=en", recEnAgain.Body.String())
	assert.Equal(t, "HIT", recEnAgain.Header().Get("X-Cache"))

	frAgain, recFrAgain := newCacheContextWithHeaders(t, e, "/i18n", map[string]string{"Accept-Language": "fr"})
	require.NoError(t, handler(frAgain))
	assert.Equal(t, "lang=fr", recFrAgain.Body.String())
	assert.Equal(t, "HIT", recFrAgain.Header().Get("X-Cache"))

	assert.Equal(t, 2, calls, "each language must have its own entry")
}

func TestCacheSeparatesAcceptEncoding(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "encoding="+c.Request().Header.Get("Accept-Encoding"))
	})

	gzipCtx, recGzip := newCacheContextWithHeaders(t, e, "/asset", map[string]string{"Accept-Encoding": "gzip"})
	require.NoError(t, handler(gzipCtx))
	assert.Equal(t, "encoding=gzip", recGzip.Body.String())
	assert.Empty(t, recGzip.Header().Get("Content-Encoding"))
	assert.Contains(t, recGzip.Header().Values("Vary"), "Accept-Encoding")

	identityCtx, recIdentity := newCacheContextWithHeaders(t, e, "/asset", map[string]string{"Accept-Encoding": "identity"})
	require.NoError(t, handler(identityCtx))
	assert.Equal(t, "encoding=identity", recIdentity.Body.String())

	gzipAgain, recGzipAgain := newCacheContextWithHeaders(t, e, "/asset", map[string]string{"Accept-Encoding": "gzip"})
	require.NoError(t, handler(gzipAgain))
	assert.Equal(t, "encoding=gzip", recGzipAgain.Body.String())
	assert.Equal(t, "HIT", recGzipAgain.Header().Get("X-Cache"))
	assert.Contains(t, recGzipAgain.Header().Values("Vary"), "Accept-Encoding")

	assert.Equal(t, 2, calls)
}

func TestCacheStatusAllowList(t *testing.T) {
	t.Run("default only caches 200", func(t *testing.T) {
		e := echo.New()
		st := store.NewMemoryStore()
		calls := 0
		cfg := Config{Store: st, TTL: time.Minute}

		handler := New(cfg)(func(c *echo.Context) error {
			calls++
			return c.String(http.StatusNotFound, "missing")
		})

		for i := 0; i < 2; i++ {
			c, rec := newCacheContext(t, e, http.MethodGet, "/missing")
			require.NoError(t, handler(c))
			assert.Equal(t, "BYPASS", rec.Header().Get("X-Cache"))
		}
		assert.Equal(t, 2, calls)
	})

	t.Run("custom allow-list includes 404", func(t *testing.T) {
		e := echo.New()
		st := store.NewMemoryStore()
		calls := 0
		cfg := Config{
			Store:                st,
			TTL:                  time.Minute,
			CacheableStatusCodes: []int{http.StatusOK, http.StatusNotFound},
		}

		handler := New(cfg)(func(c *echo.Context) error {
			calls++
			return c.String(http.StatusNotFound, "missing")
		})

		first, rec1 := newCacheContext(t, e, http.MethodGet, "/missing")
		require.NoError(t, handler(first))
		assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))

		second, rec2 := newCacheContext(t, e, http.MethodGet, "/missing")
		require.NoError(t, handler(second))
		assert.Equal(t, http.StatusNotFound, rec2.Code)
		assert.Equal(t, "missing", rec2.Body.String())
		assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
		assert.Equal(t, 1, calls)
	})
}

func TestCacheShouldCacheVeto(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0
	seen := 0
	cfg := Config{
		Store: st,
		TTL:   time.Minute,
		ShouldCache: func(c *echo.Context, entry *store.Entry) bool {
			seen++
			return false
		},
	}

	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 2; i++ {
		c, rec := newCacheContext(t, e, http.MethodGet, "/veto")
		require.NoError(t, handler(c))
		assert.Equal(t, "BYPASS", rec.Header().Get("X-Cache"))
	}

	assert.Equal(t, 2, calls)
	assert.Equal(t, 2, seen, "the hook runs for every response that passes the built-in policy")
}

func TestCacheRequestDirectivesBypass(t *testing.T) {
	for _, headers := range []map[string]string{
		{"Cache-Control": "no-store"},
		{"Cache-Control": "no-cache"},
		{"Pragma": "no-cache"},
		{"Cache-Control": "max-age=0, no-cache"},
	} {
		t.Run(headers["Cache-Control"]+headers["Pragma"], func(t *testing.T) {
			e := echo.New()
			st := store.NewMemoryStore()
			calls := 0
			cfg := Config{Store: st, TTL: time.Minute}

			handler := New(cfg)(func(c *echo.Context) error {
				calls++
				return c.String(http.StatusOK, "ok")
			})

			for i := 0; i < 2; i++ {
				c, rec := newCacheContextWithHeaders(t, e, "/directive", headers)
				require.NoError(t, handler(c))
				assert.Equal(t, "ok", rec.Body.String())
			}
			assert.Equal(t, 2, calls)
		})
	}
}

func TestCacheStripsHopByHopHeaders(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		c.Response().Header().Set("Connection", "X-Hop")
		c.Response().Header().Set("X-Hop", "secret")
		c.Response().Header().Set("Keep-Alive", "timeout=5")
		c.Response().Header().Set("Upgrade", "h2c")
		c.Response().Header().Set("X-Keep", "kept")
		return c.String(http.StatusOK, "ok")
	})

	first, rec1 := newCacheContext(t, e, http.MethodGet, "/hop")
	require.NoError(t, handler(first))
	assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))
	assert.Equal(t, "kept", rec1.Header().Get("X-Keep"))

	entry := assertCached(t, st, cfg, first)
	for _, name := range []string{"Connection", "X-Hop", "Keep-Alive", "Upgrade"} {
		assert.Empty(t, entry.Header[name], "%s must not be stored", name)
		assert.Empty(t, rec1.Header().Get(name), "%s must not be sent on a MISS", name)
	}

	second, rec2 := newCacheContext(t, e, http.MethodGet, "/hop")
	require.NoError(t, handler(second))
	assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
	for _, name := range []string{"Connection", "X-Hop", "Keep-Alive", "Upgrade"} {
		assert.Empty(t, rec2.Header().Get(name), "%s must not be replayed", name)
	}
	assert.Equal(t, "kept", rec2.Header().Get("X-Keep"))
}

func TestCacheSkipsResponsesWithSetCookieButKeepsDateOut(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	cfg := Config{Store: st, TTL: time.Minute}

	handler := New(cfg)(func(c *echo.Context) error {
		c.Response().Header().Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
		return c.String(http.StatusOK, "ok")
	})

	first, _ := newCacheContext(t, e, http.MethodGet, "/date")
	require.NoError(t, handler(first))

	entry := assertCached(t, st, cfg, first)
	assert.Empty(t, entry.Header["Date"], "Date must be regenerated, not replayed")
}

func TestCacheKeyIsStableAndHeaderSensitive(t *testing.T) {
	e := echo.New()
	cfg := resolvedConfig(Config{
		Store:       store.NewMemoryStore(),
		VaryHeaders: []string{"Accept-Encoding", "Accept-Language"},
	})

	plain, _ := newCacheContextWithHeaders(t, e, "/x", map[string]string{"Accept-Encoding": "gzip"})
	gzip := cacheKey(plain, cfg)

	same, _ := newCacheContextWithHeaders(t, e, "/x", map[string]string{"Accept-Encoding": "gzip"})
	assert.Equal(t, gzip, cacheKey(same, cfg), "the key must be deterministic")

	identityCtx, _ := newCacheContextWithHeaders(t, e, "/x", map[string]string{"Accept-Encoding": "identity"})
	assert.NotEqual(t, gzip, cacheKey(identityCtx, cfg))

	languageCtx, _ := newCacheContextWithHeaders(t, e, "/x", map[string]string{"Accept-Encoding": "gzip", "Accept-Language": "fr"})
	assert.NotEqual(t, gzip, cacheKey(languageCtx, cfg))

	otherPath, _ := newCacheContextWithHeaders(t, e, "/y", map[string]string{"Accept-Encoding": "gzip"})
	assert.NotEqual(t, gzip, cacheKey(otherPath, cfg))

	assert.NotContains(t, gzip, "gzip", "header values must not leak into the key")
}

func TestCacheKeyWithoutVaryIsBase(t *testing.T) {
	e := echo.New()
	cfg := Config{
		Store:        store.NewMemoryStore(),
		VaryHeaders:  []string{},
		KeyGenerator: DefaultKeyGenerator,
	}

	c, _ := newCacheContextWithHeaders(t, e, "/x", map[string]string{"Accept-Encoding": "gzip"})
	assert.Equal(t, DefaultKeyGenerator(c), cacheKey(c, cfg))
}

func TestHasDirective(t *testing.T) {
	assert.True(t, hasDirective("private", "private"))
	assert.True(t, hasDirective("max-age=60, private", "private"))
	assert.True(t, hasDirective(`private="Set-Cookie"`, "private"))
	assert.False(t, hasDirective("public", "private"))
	assert.False(t, hasDirective("", "private"))
	assert.False(t, hasDirective("private-data", "private"))
}

func TestVaryAllowed(t *testing.T) {
	allowed := []string{"Accept-Encoding"}
	assert.True(t, varyAllowed(nil, allowed))
	assert.True(t, varyAllowed([]string{"Accept-Encoding"}, allowed))
	assert.True(t, varyAllowed([]string{"accept-encoding"}, allowed))
	assert.False(t, varyAllowed([]string{"Accept-Encoding, Accept-Language"}, allowed))
	assert.False(t, varyAllowed([]string{"Accept-Language"}, allowed))
	assert.False(t, varyAllowed([]string{"*"}, allowed))
	assert.False(t, varyAllowed([]string{"Accept-Encoding, *"}, allowed))
}

func TestKeyHeadersDeduplicates(t *testing.T) {
	got := keyHeaders(Config{
		VaryHeaders:        []string{"Accept-Encoding", "accept-encoding", " Accept-Language ", ""},
		CacheAuthenticated: true,
	})
	assert.Equal(t, []string{"Accept-Encoding", "Accept-Language", "Authorization", "Cookie"}, got)
}
