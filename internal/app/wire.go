package app

import (
	"context"

	"connectrpc.com/connect"
)

// wire constructs the services this role serves. It grows with each
// milestone; every service is registered here and nowhere else.
func (a *App) wire(ctx context.Context) error {
	return nil
}

// interceptors returns the request-path interceptors (authentication,
// tenancy, audit) once those subsystems exist.
func (a *App) interceptors() []connect.Interceptor {
	return nil
}
