package httpkit

import (
	"mime"
	"net/http"
	"strings"
)

// ErrorCodeCrossSiteRequest and related constants are standard error codes for
// browser security guardrail responses.
const (
	ErrorCodeCrossSiteRequest        = "cross_site_request"
	ErrorCodeMissingContentType      = "missing_content_type"
	ErrorCodeUnsupportedContentType  = "unsupported_content_type"
	defaultContentSecurityPolicy     = "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'"
	defaultPermissionsPolicy         = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
	defaultCrossOriginOpenerPolicy   = "same-origin"
	defaultCrossOriginResourcePolicy = "same-origin"
)

// WithBrowserSecurityDefaults applies the shared safe-by-default HTTP guardrail
// set for platform APIs. It intentionally uses low-risk primitives that work
// for Connect, JSON REST handlers, OAuth form posts, and internal CLIs.
func WithBrowserSecurityDefaults(next http.Handler) http.Handler {
	return WithSecurityHeaders(WithFetchMetadataProtection(WithRequestContentTypeValidation(next)))
}

// WithSecurityHeaders sets browser hardening headers unless a handler has
// already supplied service-specific values.
func WithSecurityHeaders(next http.Handler) http.Handler {
	next = nonNilHandler(next)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		setHeaderIfEmpty(header, "X-Content-Type-Options", "nosniff")
		setHeaderIfEmpty(header, "X-Frame-Options", "DENY")
		setHeaderIfEmpty(header, "Referrer-Policy", "no-referrer")
		setHeaderIfEmpty(header, "Content-Security-Policy", defaultContentSecurityPolicy)
		setHeaderIfEmpty(header, "Permissions-Policy", defaultPermissionsPolicy)
		setHeaderIfEmpty(header, "Cross-Origin-Opener-Policy", defaultCrossOriginOpenerPolicy)
		setHeaderIfEmpty(header, "Cross-Origin-Resource-Policy", defaultCrossOriginResourcePolicy)
		next.ServeHTTP(writer, request)
	})
}

// WithFetchMetadataProtection rejects cross-site browser writes. Non-browser
// clients and webhooks generally omit Sec-Fetch-Site, so they continue through
// the normal auth and signature checks.
func WithFetchMetadataProtection(next http.Handler) http.Handler {
	next = nonNilHandler(next)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if unsafeHTTPMethod(request.Method) && strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")), "cross-site") {
			WriteError(writer, http.StatusForbidden, ErrorCodeCrossSiteRequest, "cross-site browser requests are not allowed")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// WithRequestContentTypeValidation rejects body-bearing unsafe requests without
// an allowed Content-Type. This catches ambiguous browser form/text posts while
// preserving JSON, Connect, protobuf, and OAuth form traffic.
func WithRequestContentTypeValidation(next http.Handler) http.Handler {
	next = nonNilHandler(next)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !unsafeHTTPMethod(request.Method) || !requestHasBody(request) {
			next.ServeHTTP(writer, request)
			return
		}

		rawContentType := strings.TrimSpace(request.Header.Get("Content-Type"))
		if rawContentType == "" {
			WriteError(writer, http.StatusUnsupportedMediaType, ErrorCodeMissingContentType, "request body requires a Content-Type header")
			return
		}
		mediaType, _, err := mime.ParseMediaType(rawContentType)
		if err != nil || !allowedRequestContentType(mediaType) {
			WriteError(writer, http.StatusUnsupportedMediaType, ErrorCodeUnsupportedContentType, "request Content-Type is not supported")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func allowedRequestContentType(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case mediaType == "application/json":
		return true
	case strings.HasSuffix(mediaType, "+json"):
		return true
	case mediaType == "application/x-www-form-urlencoded":
		return true
	case mediaType == "multipart/form-data":
		return true
	case mediaType == "application/protobuf", mediaType == "application/x-protobuf", mediaType == "application/proto":
		return true
	case strings.HasSuffix(mediaType, "+proto"):
		return true
	case mediaType == "application/grpc":
		return true
	case strings.HasPrefix(mediaType, "application/grpc+"):
		return true
	case mediaType == "application/grpc-web", mediaType == "application/grpc-web-text":
		return true
	case strings.HasPrefix(mediaType, "application/grpc-web+"), strings.HasPrefix(mediaType, "application/grpc-web-text+"):
		return true
	case mediaType == "application/octet-stream":
		return true
	case mediaType == "application/x-ndjson":
		return true
	default:
		return false
	}
}

func setHeaderIfEmpty(header http.Header, key string, value string) {
	if header.Get(key) == "" {
		header.Set(key, value)
	}
}

func unsafeHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

func requestHasBody(request *http.Request) bool {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return false
	}
	if request.ContentLength > 0 || request.ContentLength == -1 {
		return true
	}
	return len(request.TransferEncoding) > 0
}

func nonNilHandler(next http.Handler) http.Handler {
	if next == nil {
		return http.DefaultServeMux
	}
	return next
}
