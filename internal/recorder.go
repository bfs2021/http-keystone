package internal

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
)

// errHijacked is returned when a handler tries to write to a response that has
// already been hijacked.
var errHijacked = errors.New("internal: response already hijacked")

// ResponseRecorder buffers a response so a middleware can inspect and modify
// it before anything is sent to the client.
//
// The recorder holds at most Limit bytes. If the handler writes more than
// that, the recorder flushes what it has and switches to pass-through mode;
// the response is streamed normally and Buffered reports false. Calling
// Flush or Hijack also disables buffering, which keeps SSE, websockets, and
// other streaming handlers working.
//
// A middleware must call Commit to send a fully buffered response, or Discard
// to throw it away (for example when the handler returns an error and Echo
// should render its own error response).
//
// ResponseRecorder implements Unwrap so it plays well with
// http.ResponseController and Echo's response-writer contract.
type ResponseRecorder struct {
	http.ResponseWriter

	// BeforeWriteHeader, when set, is called once just before the status line
	// and headers are written to the real writer. It is the last chance to add
	// headers such as ETag or X-Cache.
	BeforeWriteHeader func(http.Header)

	limit       int
	status      int
	buf         bytes.Buffer
	wroteHeader bool
	headerSent  bool
	passthrough bool
	overflowed  bool
	hijacked    bool
	discarded   bool
}

// NewResponseRecorder wraps w and buffers up to limit bytes. A limit <= 0
// disables buffering entirely: the first write goes straight to w.
func NewResponseRecorder(w http.ResponseWriter, limit int) *ResponseRecorder {
	return &ResponseRecorder{
		ResponseWriter: w,
		limit:          limit,
		status:         http.StatusOK,
	}
}

// WriteHeader records the status code. It is not sent to the client until
// Commit, a buffer overflow, or Flush.
func (r *ResponseRecorder) WriteHeader(code int) {
	if r.hijacked || r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	if r.passthrough {
		r.writeHeader()
	}
}

// Write stores the body in the buffer, or streams it once the limit is
// exceeded.
func (r *ResponseRecorder) Write(p []byte) (int, error) {
	if r.hijacked {
		return 0, errHijacked
	}
	if r.discarded {
		return len(p), nil
	}
	if !r.wroteHeader {
		r.wroteHeader = true
	}

	if r.passthrough {
		return r.ResponseWriter.Write(p)
	}

	if r.limit <= 0 || r.buf.Len()+len(p) > r.limit {
		r.overflowed = true
		if err := r.startPassthrough(); err != nil {
			return 0, err
		}
		return r.ResponseWriter.Write(p)
	}

	return r.buf.Write(p)
}

// Commit flushes a buffered response to the client. It is a no-op when the
// response was streamed, hijacked, or discarded.
func (r *ResponseRecorder) Commit() error {
	if r.hijacked || r.discarded || r.passthrough {
		return nil
	}
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.startPassthrough()
}

// Discard drops the buffered response and prevents it from being committed.
// Headers already set on the underlying writer are left alone.
func (r *ResponseRecorder) Discard() {
	r.discarded = true
	r.buf.Reset()
}

// Flush sends the status, headers, and buffered body, then flushes the
// underlying writer. It permanently disables buffering for this response.
func (r *ResponseRecorder) Flush() {
	_ = r.FlushError()
}

// FlushError is the http.ResponseController-compatible variant of Flush.
func (r *ResponseRecorder) FlushError() error {
	if r.hijacked || r.discarded {
		return nil
	}
	if !r.passthrough {
		if err := r.startPassthrough(); err != nil {
			return err
		}
	}
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// Hijack lets the handler take over the connection (websockets). Buffered
// data is discarded and no HTTP response is written.
func (r *ResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if r.hijacked {
		return nil, nil, errHijacked
	}
	r.hijacked = true
	r.buf.Reset()
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

// Push implements http.Pusher when the underlying writer supports HTTP/2
// server push.
func (r *ResponseRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the wrapped writer. Echo v5 uses this to reach the original
// *echo.Response and http.ResponseController uses it for optional interfaces.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Status returns the recorded status code.
func (r *ResponseRecorder) Status() int {
	return r.status
}

// Body returns a copy of the buffered body.
func (r *ResponseRecorder) Body() []byte {
	out := make([]byte, r.buf.Len())
	copy(out, r.buf.Bytes())
	return out
}

// Buffered reports whether the response is still being buffered.
func (r *ResponseRecorder) Buffered() bool {
	return !r.passthrough && !r.hijacked && !r.discarded
}

// Passthrough reports whether the response was streamed because it exceeded
// the buffer limit or the handler flushed it.
func (r *ResponseRecorder) Passthrough() bool {
	return r.passthrough
}

// Overflowed reports whether the buffer limit was exceeded.
func (r *ResponseRecorder) Overflowed() bool {
	return r.overflowed
}

// Hijacked reports whether the handler took over the connection.
func (r *ResponseRecorder) Hijacked() bool {
	return r.hijacked
}

// startPassthrough writes the status, headers, and buffered body to the
// underlying writer and disables further buffering.
func (r *ResponseRecorder) startPassthrough() error {
	if r.passthrough {
		return nil
	}
	r.passthrough = true
	r.wroteHeader = true

	r.writeHeader()

	if r.buf.Len() > 0 {
		_, err := r.ResponseWriter.Write(r.buf.Bytes())
		r.buf.Reset()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ResponseRecorder) writeHeader() {
	if r.headerSent {
		return
	}
	r.headerSent = true
	if r.BeforeWriteHeader != nil {
		r.BeforeWriteHeader(r.Header())
	}
	r.ResponseWriter.WriteHeader(r.status)
}
