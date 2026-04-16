package observability

import (
	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
)

// ConnectOTelInterceptor returns a Connect interceptor that emits
// OpenTelemetry spans and metrics for every RPC. Spans include the proto
// service and method names, request/response sizes, and error codes mapped
// to OTel status.
//
// Use this on both server handlers and clients:
//
//	interceptor := observability.ConnectOTelInterceptor()
//	mux.Handle(svcconnect.NewFooServiceHandler(&handler{},
//	    connect.WithInterceptors(interceptor),
//	))
func ConnectOTelInterceptor(opts ...otelconnect.Option) connect.Interceptor {
	interceptor, err := otelconnect.NewInterceptor(opts...)
	if err != nil {
		// otelconnect.NewInterceptor only errors on invalid Option
		// combinations. With default options this cannot fail, so panic
		// during startup rather than hiding a misconfiguration.
		panic("otelconnect: " + err.Error())
	}
	return interceptor
}
