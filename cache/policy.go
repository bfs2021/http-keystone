package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// hopByHopHeaders are connection-specific and must not be stored or replayed
// from a cache.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// requestCacheable reports whether the request is safe to cache. Method and
// Skipper are handled by the middleware before this check.
func requestCacheable(c *echo.Context, cfg Config) bool {
	req := c.Request()

	if !cfg.CacheAuthenticated {
		if req.Header.Get("Authorization") != "" ||
			req.Header.Get("Proxy-Authorization") != "" ||
			req.Header.Get("Cookie") != "" {
			return false
		}
	}

	cacheControl := req.Header.Get("Cache-Control")
	if hasDirective(cacheControl, "no-store") || hasDirective(cacheControl, "no-cache") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(req.Header.Get("Pragma")), "no-cache") {
		return false
	}

	return true
}

// responseCacheable applies the default safety policy to a response. It runs
// before headers are sanitized, so it can still see Set-Cookie and Cache-Control.
func responseCacheable(c *echo.Context, entry *store.Entry, cfg Config) bool {
	if !statusAllowed(entry.Status, cfg.CacheableStatusCodes) {
		return false
	}

	header := http.Header(entry.Header)

	if header.Get("Set-Cookie") != "" {
		return false
	}

	cacheControl := header.Get("Cache-Control")
	if hasDirective(cacheControl, "no-store") ||
		hasDirective(cacheControl, "private") ||
		hasDirective(cacheControl, "no-cache") {
		return false
	}

	if !varyAllowed(header.Values("Vary"), keyHeaders(cfg)) {
		return false
	}

	if cfg.ShouldCache != nil && !cfg.ShouldCache(c, entry) {
		return false
	}

	return true
}

// cacheKey returns the base key plus a stable digest of the configured Vary
// headers (and credentials when CacheAuthenticated is enabled).
func cacheKey(c *echo.Context, cfg Config) string {
	base := cfg.KeyGenerator(c)
	headers := keyHeaders(cfg)
	if len(headers) == 0 {
		return base
	}

	hash := sha256.New()
	for _, name := range headers {
		hash.Write([]byte(strings.ToLower(name)))
		hash.Write([]byte{0})
		hash.Write([]byte(strings.Join(c.Request().Header.Values(name), "\n")))
		hash.Write([]byte{0})
	}

	return base + ":vary:" + hex.EncodeToString(hash.Sum(nil)[:16])
}

// keyHeaders returns the request headers that must be part of the cache key.
func keyHeaders(cfg Config) []string {
	headers := append([]string(nil), cfg.VaryHeaders...)
	if cfg.CacheAuthenticated {
		headers = append(headers, "Authorization", "Cookie")
	}

	seen := make(map[string]struct{}, len(headers))
	out := make([]string, 0, len(headers))
	for _, name := range headers {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	return out
}

// varyAllowed reports whether every header named by Vary is part of the cache
// key. Vary: * always disables caching.
func varyAllowed(varyValues, allowed []string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[strings.ToLower(name)] = struct{}{}
	}

	for _, value := range varyValues {
		for _, part := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "*" {
				return false
			}
			if _, ok := allowedSet[name]; !ok {
				return false
			}
		}
	}
	return true
}

// sanitizeHeader returns a copy of h without hop-by-hop headers, cookies, and
// response metadata that must be regenerated on replay.
func sanitizeHeader(h http.Header) http.Header {
	out := h.Clone()

	for _, connection := range out.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		out.Del(name)
	}
	out.Del("Set-Cookie")
	out.Del("Date")
	out.Del("Age")
	out.Del("X-Cache")

	return out
}

// ensureVary advertises every header that participates in the cache key.
func ensureVary(h http.Header, names []string) {
	existing := make(map[string]struct{})
	for _, value := range h.Values("Vary") {
		for _, part := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name != "" {
				existing[name] = struct{}{}
			}
		}
	}

	for _, name := range names {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if key == "" || key == "*" {
			continue
		}
		if _, ok := existing[key]; ok {
			continue
		}
		h.Add("Vary", name)
		existing[key] = struct{}{}
	}
}

func statusAllowed(status int, allowed []int) bool {
	for _, code := range allowed {
		if status == code {
			return true
		}
	}
	return false
}

// hasDirective reports whether a comma-separated header value contains the
// given directive, ignoring any directive value (for example private="Set-Cookie").
func hasDirective(value, directive string) bool {
	for _, part := range strings.Split(value, ",") {
		token := strings.TrimSpace(part)
		if index := strings.IndexByte(token, '='); index >= 0 {
			token = strings.TrimSpace(token[:index])
		}
		if strings.EqualFold(token, directive) {
			return true
		}
	}
	return false
}
