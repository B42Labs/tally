// Package signin decides who may read the console and records who that was for
// the access log.
//
// A sign-in is a func(http.Handler) http.Handler, which is the type the router
// takes. A middleware can answer a request itself, and an OIDC sign-in needs
// that for its redirects and its callback route, which a check of a user name
// and a password could not express. Basic is the one implementation today. The
// OIDC work of #169 and #233 provides another such function that calls
// WithOperator, and nothing under internal/console/httpui outside the router
// knows how a sign-in works.
package signin

import "context"

// operatorKey is the context key of the signed-in operator's name.
type operatorKey struct{}

// WithOperator returns ctx carrying the name of the operator who signed in.
func WithOperator(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, operatorKey{}, name)
}

// OperatorFrom returns the name WithOperator stored, and the empty string for a
// context no sign-in touched.
func OperatorFrom(ctx context.Context) string {
	name, _ := ctx.Value(operatorKey{}).(string)
	return name
}
