package soak

import (
	"context"
	"net/http"
)

// The fake environment has no browser UI: phase 16 is skipped against it.
func (e *fakeEnv) WebURL(context.Context, NodeRef) (string, error) { return "", nil }
func (e *fakeEnv) WebClient() *http.Client                         { return nil }
