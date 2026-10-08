package vfs_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
