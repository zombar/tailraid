package main

import (
	"testing"

	"go.uber.org/goleak"
)

// The CLI tests start nodes and services; none may outlive its test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
