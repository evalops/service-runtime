package httpkit

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
)

// Versioned is implemented by response payloads that carry an aggregate version for ETag headers.
type Versioned interface {
	GetVersion() int64
}

// CaptureResponseWriter wraps an http.ResponseWriter to capture the status code and body.
type CaptureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
}

// WriteJSON writes a JSON response with the given HTTP status and value.
func WriteJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = jsonNewEncoder(writer).Encode(value)
}

// WriteMutationJSON writes a JSON response with X-Change-Seq and ETag headers for a mutation result.
func WriteMutationJSON(writer http.ResponseWriter, status int, payload any, sequence int64) {
	writer.Header().Set("X-Change-Seq", strconv.FormatInt(sequence, 10))
	if versioned, ok := payload.(Versioned); ok {
		SetETag(writer, versioned.GetVersion())
	}
	WriteJSON(writer, status, payload)
}

// SetETag sets the ETag response header to the quoted version number.
func SetETag(writer http.ResponseWriter, version int64) {
	writer.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

// NewCaptureResponseWriter creates a CaptureResponseWriter wrapping the given writer.
func NewCaptureResponseWriter(writer http.ResponseWriter) *CaptureResponseWriter {
	return &CaptureResponseWriter{
		ResponseWriter: writer,
		statusCode:     http.StatusOK,
	}
}

// StatusCode returns the HTTP status code written to the response.
func (writer *CaptureResponseWriter) StatusCode() int {
	return writer.statusCode
}

// BodyBytes returns a copy of the response body written so far.
func (writer *CaptureResponseWriter) BodyBytes() []byte {
	return append([]byte(nil), writer.body.Bytes()...)
}

// WriteHeader records the status code and delegates to the underlying writer.
func (writer *CaptureResponseWriter) WriteHeader(statusCode int) {
	if writer.wroteHeader {
		return
	}
	writer.wroteHeader = true
	writer.statusCode = statusCode
	writer.ResponseWriter.WriteHeader(statusCode)
}

func (writer *CaptureResponseWriter) Write(body []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	_, _ = writer.body.Write(body)
	return writer.ResponseWriter.Write(body)
}

// Flush delegates to the underlying writer if it implements http.Flusher.
func (writer *CaptureResponseWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack delegates to the underlying writer if it implements http.Hijacker.
func (writer *CaptureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

// Push delegates to the underlying writer if it implements http.Pusher.
func (writer *CaptureResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

// ReadFrom copies from the reader into the response writer.
func (writer *CaptureResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(writer, reader)
}

type jsonEncoder interface {
	Encode(v any) error
}

var jsonNewEncoder = newJSONEncoder
