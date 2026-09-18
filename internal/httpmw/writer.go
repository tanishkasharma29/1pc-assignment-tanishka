package httpmw

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// observer wraps a ResponseWriter to record what was sent, while passing writes
// straight through.
//
// This is the opposite of the caching middleware's recorder, which buffers. Here
// the response must not be delayed: this wrapper only observes.
type observer struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func newObserver(w http.ResponseWriter) *observer {
	return &observer{ResponseWriter: w, status: http.StatusOK}
}

func (o *observer) WriteHeader(status int) {
	if o.wroteHeader {
		return
	}
	o.status = status
	o.wroteHeader = true
	o.ResponseWriter.WriteHeader(status)
}

func (o *observer) Write(p []byte) (int, error) {
	if !o.wroteHeader {
		o.WriteHeader(http.StatusOK)
	}
	n, err := o.ResponseWriter.Write(p)
	o.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so wrapping
// does not silently disable flushing or deadline control for handlers that use
// the modern API.
func (o *observer) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observer) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *observer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := o.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("httpmw: underlying ResponseWriter does not support hijacking")
}
