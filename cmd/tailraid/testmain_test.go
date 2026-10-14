package main

import (
	"testing"

	"github.com/zalando/go-keyring"
	"go.uber.org/goleak"
)

// The CLI tests start nodes and services; none may outlive its test. A node keeps the
// cluster key in the keychain (docs/19), so the tests use go-keyring's in-memory one: no
// test touches the real keychain.
func TestMain(m *testing.M) {
	keyring.MockInit()
	goleak.VerifyTestMain(m)
}
