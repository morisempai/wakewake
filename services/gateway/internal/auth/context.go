package auth

import "context"

// subjectKey carries the authenticated subject (the token's `sub`) through the request context, so
// the rate limiter downstream can key on the caller's identity rather than only their IP.
type subjectKey struct{}

// WithSubject returns a context carrying the authenticated subject.
func WithSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, subjectKey{}, sub)
}

// SubjectFromContext returns the authenticated subject, or "" if the request was not authenticated
// (for example the public Stripe webhook, which carries no JWT).
func SubjectFromContext(ctx context.Context) string {
	s, _ := ctx.Value(subjectKey{}).(string)
	return s
}
