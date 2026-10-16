// Package client is the Go SDK for the rate limiter service. A service that
// wants its callers limited asks the limiter "may this key consume now?" through
// this package, so it does not have to write the gRPC client, the timeout and the
// error handling itself.
//
// The SDK plugs in on the server side, in front of the protected service: as a
// gRPC unary server interceptor or as net/http middleware, on the requests that
// arrive. There is no interceptor for outgoing calls.
//
// When the limiter denies a request, the interceptor answers
// codes.ResourceExhausted with a RetryInfo detail and the middleware answers 429
// with a Retry-After header. A denial is the limiter's normal answer, not an
// error.
//
// When the limiter cannot answer (Unavailable or DeadlineExceeded), the fail mode
// decides. With FailOpen the request goes through and a warning is logged. With
// FailClosed it is rejected: the interceptor returns codes.Unavailable and the
// middleware answers 503. The fail mode has no default, because either default
// is a quiet mistake: New returns an error when it is not set. The reasoning is
// in ADR 0003 in the repository's docs/adr directory.
//
// To use it, create a Client with the limiter's address, a namespace that names
// the protected service, a fail mode and transport credentials. Then wrap the
// protected service with the interceptor or the middleware, giving it the policy
// to apply and a KeyFunc that says what is being limited: KeyFromMetadata and
// KeyFromHeader read a request value, KeyFromPeer uses the caller's address.
// Close the Client when the service stops. If the limiter's address resolves to
// several replicas, the Client spreads its calls over all of them.
//
// The module is at v0, so this API can change before v1.0.0.
package client
