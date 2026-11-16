package cache

import (
	"net/http"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// replay writes a cached entry to the response.
//
// Stored headers are sanitized again on the way out: hop-by-hop headers and
// cookies never reach a different client, and Vary always advertises every
// header the key was built from.
func replay(c *echo.Context, entry *store.Entry, config Config) error {
	// Cast map to http.Header for the .Get() helpers.
	headers := sanitizeHeader(http.Header(entry.Header))
	ensureVary(headers, config.VaryHeaders)

	clientETag := c.Request().Header.Get("If-None-Match")
	serverETag := headers.Get("ETag")

	if serverETag != "" && clientETag == serverETag {
		return c.NoContent(http.StatusNotModified)
	}

	// Transfer headers to the actual response.
	for name, values := range headers {
		for _, value := range values {
			c.Response().Header().Add(name, value)
		}
	}
	c.Response().Header().Set("X-Cache", "HIT")

	// Ensure Content-Type is provided or use the cached one.
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return c.Blob(entry.Status, contentType, entry.Body)
}
