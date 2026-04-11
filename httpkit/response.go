package httpkit

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
)

type Versioned interface {
	GetVersion() int64
}

type CaptureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
}

func WriteJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = jsonNewEncoder(writer).Encode(value)
}

func WriteMutationJSON(writer http.ResponseWriter, status int, payload any, sequence int64) {
	writer.Header().Set("X-Change-Seq", strconv.FormatInt(sequence, 10))
	if versioned, ok := payload.(Versioned); ok {
		SetETag(writer, versioned.GetVersion())
	}
	WriteJSON(writer, status, payload)
}

func SetETag(writer http.ResponseWriter, version int64) {
	writer.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

func NewCaptureResponseWriter(writer http.ResponseWriter) *CaptureResponseWriter {
	return &CaptureResponseWriter{
		ResponseWriter: writer,
		statusCode:     http.StatusOK,
	}
}

func (writer *CaptureResponseWriter) StatusCode() int {
	return writer.statusCode
}

func (writer *CaptureResponseWriter) BodyBytes() []byte {
	return append([]byte(nil), writer.body.Bytes()...)
}

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

func (writer *CaptureResponseWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *CaptureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (writer *CaptureResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (writer *CaptureResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(writer, reader)
}

type jsonEncoder interface {
	Encode(v any) error
}

var jsonNewEncoder = func(writer io.Writer) jsonEncoder {
	return newJSONEncoder(writer)
}
