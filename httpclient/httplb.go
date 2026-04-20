package httpclient

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/bufbuild/httplb"
)

const (
	DefaultRequestTimeout      = 30 * time.Second
	DefaultIdleConnTimeout     = 90 * time.Second
	DefaultMaxIdleConns        = 64
	DefaultMaxIdleConnsPerHost = 16
	DefaultMaxConnsPerHost     = 32
)

// LoadBalancedOptions controls EvalOps' default outbound HTTP client.
type LoadBalancedOptions struct {
	RequestTimeout        time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	MaxResponseHeaderSize int
	TLSClientConfig       *tls.Config
	TLSHandshakeTimeout   time.Duration
	DisableCompression    bool
}

// LoadBalancedClient owns an httplb client and exposes its standard *http.Client.
type LoadBalancedClient struct {
	*http.Client

	client *httplb.Client
}

var sharedDefaultClient = NewLoadBalanced(LoadBalancedOptions{})

// DefaultClient returns a shared load-balanced client for nil-client fallbacks.
func DefaultClient() *http.Client {
	return sharedDefaultClient.Client
}

// NewLoadBalanced creates an HTTP client with DNS-backed client-side balancing.
func NewLoadBalanced(options LoadBalancedOptions) *LoadBalancedClient {
	options = options.withDefaults()
	lbOptions := []httplb.ClientOption{
		httplb.WithRequestTimeout(options.RequestTimeout),
		httplb.WithIdleConnectionTimeout(options.IdleConnTimeout),
		httplb.WithTransport("http", loadBalancedTransport{options: options}),
		httplb.WithTransport("https", loadBalancedTransport{options: options}),
	}
	if options.TLSClientConfig != nil {
		lbOptions = append(
			lbOptions,
			httplb.WithTLSConfig(options.TLSClientConfig, options.TLSHandshakeTimeout),
		)
	}
	if options.MaxResponseHeaderSize > 0 {
		lbOptions = append(lbOptions, httplb.WithMaxResponseHeaderBytes(options.MaxResponseHeaderSize))
	}
	if options.DisableCompression {
		lbOptions = append(lbOptions, httplb.WithDisableCompression(true))
	}

	client := httplb.NewClient(lbOptions...)
	return &LoadBalancedClient{
		Client: client.Client,
		client: client,
	}
}

// Close releases resolver and transport resources owned by the httplb client.
func (c *LoadBalancedClient) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

func (o LoadBalancedOptions) withDefaults() LoadBalancedOptions {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.IdleConnTimeout <= 0 {
		o.IdleConnTimeout = DefaultIdleConnTimeout
	}
	if o.MaxIdleConns <= 0 {
		o.MaxIdleConns = DefaultMaxIdleConns
	}
	if o.MaxIdleConnsPerHost <= 0 {
		o.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if o.MaxConnsPerHost <= 0 {
		o.MaxConnsPerHost = DefaultMaxConnsPerHost
	}
	return o
}

type loadBalancedTransport struct {
	options LoadBalancedOptions
}

func (t loadBalancedTransport) NewRoundTripper(
	_ string,
	_ string,
	config httplb.TransportConfig,
) httplb.RoundTripperResult {
	transport := &http.Transport{
		Proxy:                  config.ProxyFunc,
		GetProxyConnectHeader:  config.ProxyConnectHeadersFunc,
		DialContext:            config.DialFunc,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           t.options.MaxIdleConns,
		MaxIdleConnsPerHost:    t.options.MaxIdleConnsPerHost,
		MaxConnsPerHost:        t.options.MaxConnsPerHost,
		IdleConnTimeout:        config.IdleConnTimeout,
		TLSHandshakeTimeout:    config.TLSHandshakeTimeout,
		TLSClientConfig:        config.TLSClientConfig,
		MaxResponseHeaderBytes: config.MaxResponseHeaderBytes,
		ExpectContinueTimeout:  time.Second,
		DisableCompression:     config.DisableCompression,
	}
	return httplb.RoundTripperResult{
		RoundTripper: transport,
		Close:        transport.CloseIdleConnections,
	}
}
