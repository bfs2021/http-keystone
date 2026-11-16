package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/internal"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// New returns an Echo middleware for caching HTTP responses.
func New(config Config) echo.MiddlewareFunc {
	// Defaults & sanity checks
	if config.Store == nil {
		panic("http-keystone: cache middleware requires a store")
	}
	if config.TTL == 0 {
		config.TTL = 5 * time.Minute
	}
	if config.KeyGenerator == nil {
		config.KeyGenerator = DefaultKeyGenerator
	}
	if config.MaxBodySize == 0 {
		config.MaxBodySize = 1024 * 1024 // 1MB
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 20 * time.Millisecond
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 5
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if config.Skipper != nil && config.Skipper(c) {
				return next(c)
			}
			if c.Request().Method != http.MethodGet {
				return next(c)
			}

			key := config.KeyGenerator(c)
			lockKey := key + ":lock"
			ctx := c.Request().Context()

			entry, err := config.Store.Get(ctx, key)
			if err == nil && entry != nil {
				return replay(c, entry)
			}

			locked, unlockFn := acquireLockWithTTL(ctx, config.Store, lockKey, 10*time.Second)
			if !locked {
				// Stampede handling is tracked separately; fall through for now.
				return next(c)
			}
			defer unlockFn()

			originalWriter := c.Response()
			recorder := internal.NewResponseRecorder(originalWriter, config.MaxBodySize)
			recorder.BeforeWriteHeader = func(h http.Header) {
				if h.Get("X-Cache") == "" {
					if recorder.Passthrough() {
						h.Set("X-Cache", "BYPASS")
					} else {
						h.Set("X-Cache", "MISS")
					}
				}
			}
			c.SetResponse(recorder)
			defer c.SetResponse(originalWriter)

			if err := next(c); err != nil {
				// Echo should render the error response; never cache a partial
				// or failed response.
				recorder.Discard()
				return err
			}

			// The response overran MaxBodySize or was flushed by the handler.
			// It has already been streamed to the client and must not be cached.
			if recorder.Passthrough() {
				return nil
			}

			cached := &store.Entry{
				Status: recorder.Status(),
				Header: recorder.Header().Clone(),
				Body:   recorder.Body(),
			}
			http.Header(cached.Header).Del("X-Cache")

			saved := false
			if cached.Status == http.StatusOK {
				hash := sha256.Sum256(cached.Body)
				etag := hex.EncodeToString(hash[:])

				http.Header(cached.Header).Set("ETag", etag)
				http.Header(cached.Header).Set("X-Cache", "MISS")

				if err := config.Store.Save(ctx, key, cached, config.TTL); err == nil {
					saved = true
					recorder.Header().Set("ETag", etag)
					recorder.Header().Set("X-Cache", "MISS")
				}
			}
			if !saved {
				recorder.Header().Set("X-Cache", "BYPASS")
			}

			return recorder.Commit()
		}
	}
}
