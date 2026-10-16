package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

// defaultTimeout is a starting value: the limiter's own Redis timeout (50 ms by
// default) plus the network. It is meant to be tuned with measurements.
const defaultTimeout = 100 * time.Millisecond

// roundRobinConfig spreads calls over every address the target resolves to.
// gRPC's own default, pick_first, uses one address and leaves the other limiter
// replicas idle; for a DNS name with several addresses that is not what anyone
// wants, and with a single address it makes no difference.
const roundRobinConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// FailMode says what the interceptor and the middleware do when the limiter
// cannot answer. There is no default: the zero value means "not set", and New
// rejects it.
type FailMode int

const (
	// FailOpen lets the request through, so a limiter outage does not take the
	// protected service down with it. Limits are not enforced meanwhile.
	FailOpen FailMode = iota + 1
	// FailClosed rejects the request, so limits hold during an outage at the cost
	// of failing requests that would have been allowed.
	FailClosed
)

// Decision is the limiter's answer to one Allow call. It is the SDK's own type,
// so a change to the wire format does not reach the callers.
type Decision struct {
	// Allowed is false when the key is over its limit. That is a normal answer,
	// not an error.
	Allowed bool
	// Remaining is how much of the limit is left.
	Remaining int64
	// RetryAfter is how long to wait before trying again; zero when allowed.
	RetryAfter time.Duration
}

// Option configures a Client.
type Option func(*config)

type config struct {
	timeout   time.Duration
	failMode  FailMode
	namespace string
	creds     credentials.TransportCredentials
}

// WithTimeout limits each call to the limiter. The default is 100 ms.
func WithTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// WithFailMode sets what happens when the limiter cannot answer. It is required.
func WithFailMode(m FailMode) Option { return func(c *config) { c.failMode = m } }

// WithNamespace names the protected service. Keys of different services do not
// collide because of it. It is required.
func WithNamespace(ns string) Option { return func(c *config) { c.namespace = ns } }

// WithTransportCredentials sets how to secure the connection to the limiter. It
// is required and has no plaintext default, so an insecure connection is always a
// choice made in the caller's code.
func WithTransportCredentials(creds credentials.TransportCredentials) Option {
	return func(c *config) { c.creds = creds }
}

// newConfig applies the options and checks them. Every problem is reported at
// once, so a caller fixes them in one go.
func newConfig(opts []Option) (config, error) {
	cfg := config{timeout: defaultTimeout}
	for _, o := range opts {
		o(&cfg)
	}

	var errs []error
	if cfg.namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	}
	if cfg.creds == nil {
		errs = append(errs, errors.New("transport credentials are required"))
	}
	if cfg.failMode != FailOpen && cfg.failMode != FailClosed {
		errs = append(errs, errors.New("fail mode is required: FailOpen or FailClosed"))
	}
	if cfg.timeout <= 0 {
		errs = append(errs, errors.New("timeout must be positive"))
	}
	if err := errors.Join(errs...); err != nil {
		return config{}, fmt.Errorf("client: invalid options: %w", err)
	}
	return cfg, nil
}

// Client asks the limiter service whether a key may proceed.
type Client struct {
	conn *grpc.ClientConn
	rl   ratelimiterv1.RateLimitServiceClient
	cfg  config
}

// New creates a Client for the limiter at target.
//
// New does no network I/O: gRPC connects on the first call. A nil error therefore
// does not mean the limiter is reachable, and the fail mode has to work from the
// very first request.
//
// If target resolves to several addresses (for example "dns:///ratelimiter:8080"
// with several limiter replicas behind the name), calls are spread over all of
// them, round robin.
func New(target string, opts ...Option) (*Client, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(cfg.creds),
		grpc.WithDefaultServiceConfig(roundRobinConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("client: create connection to %q: %w", target, err)
	}
	return newClient(conn, cfg), nil
}

// newClient wraps an existing connection. Tests use it to supply an in-memory
// one without adding anything to the exported API.
func newClient(conn *grpc.ClientConn, cfg config) *Client {
	return &Client{conn: conn, rl: ratelimiterv1.NewRateLimitServiceClient(conn), cfg: cfg}
}

// Allow asks whether key may consume one unit of policy now, and consumes it if
// so. The call is bounded by the Client's timeout, or by ctx if that ends sooner.
//
// A denial is a nil error with Decision.Allowed false. An error means no decision
// could be made; it is the gRPC error as received, and applying the fail mode is
// left to the interceptor and the middleware.
func (c *Client) Allow(ctx context.Context, policy, key string) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.timeout)
	defer cancel()

	resp, err := c.rl.Allow(ctx, &ratelimiterv1.AllowRequest{
		Namespace: c.cfg.namespace,
		Policy:    policy,
		Key:       key,
	})
	if err != nil {
		return Decision{}, err
	}
	return Decision{
		Allowed:    resp.GetAllowed(),
		Remaining:  resp.GetRemaining(),
		RetryAfter: resp.GetRetryAfter().AsDuration(),
	}, nil
}

// Close releases the connection. Call it when the service stops.
func (c *Client) Close() error {
	return c.conn.Close()
}
