package httpkit

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
)

// Versioned is implemented by domain objects that carry a version field.
type Versioned interface {
	GetVersion() int64
}

// CaptureResponseWriter is an http.ResponseWriter that captures status code and body for later inspection.
type CaptureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
}

// WriteJSON writes a JSON-encoded value with the given HTTP status code.
func WriteJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = jsonNewEncoder(writer).Encode(value)
}

// WriteMutationJSON writes a JSON mutation response with X-Change-Seq and ETag headers.
func WriteMutationJSON(writer http.ResponseWriter, status int, payload any, sequence int64) {
	writer.Header().Set("X-Change-Seq", strconv.FormatInt(sequence, 10))
	if versioned, ok := payload.(Versioned); ok {
		SetETag(writer, versioned.GetVersion())
	}
	WriteJSON(writer, status, payload)
}

// SetETag sets the ETag response header to the given numeric version.
func SetETag(writer http.ResponseWriter, version int64) {
	writer.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

// NewCaptureResponseWriter wraps writer in a CaptureResponseWriter.
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

// BodyBytes returns a copy of the response body bytes written so far.
func (writer *CaptureResponseWriter) BodyBytes() []byte {
	return append([]byte(nil), writer.body.Bytes()...)
}

// WriteHeader records the status code and delegates to the underlying ResponseWriter.
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

// Flush implements http.Flusher by delegating to the underlying writer.
func (writer *CaptureResponseWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker by delegating to the underlying writer.
func (writer *CaptureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

// Push implements http.Pusher by delegating to the underlying writer.
func (writer *CaptureResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

// ReadFrom implements io.ReaderFrom by copying from reader through Write.
func (writer *CaptureResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	tee := io.TeeReader(reader, &writer.body)
	n, err := io.Copy(writer.ResponseWriter, tee)
	if n > 0 && !writer.wroteHeader {
		writer.wroteHeader = true
		writer.statusCode = http.StatusOK
	}
	return n, err
}

type jsonEncoder interface {
	Encode(v any) error
}

var jsonNewEncoder = newJSONEncoder
