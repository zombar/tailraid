package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// s3Creds is the credentials of S3 node name, from the secrets store through this
// device's node (docs/19 §6): `secret set s3/<name>/access_key` and
// `secret set s3/<name>/secret_key` come first.
func s3Creds(cmd *cobra.Command, name string) (s3.Credentials, error) {
	access, secret, ok := cluster.S3SecretNames(name)
	if !ok {
		return s3.Credentials{}, fmt.Errorf("S3 node name %q: lower-case letters, digits and . _ - (it names the secrets %s<name>/access_key and secret_key)", name, cluster.S3SecretPrefix)
	}
	var c s3.Credentials
	for _, f := range []struct {
		name string
		into *string
	}{{access, &c.AccessKey}, {secret, &c.SecretKey}} {
		v, err := storedSecret(cmd, f.name)
		if err != nil {
			return c, fmt.Errorf("the credentials of %s from the secrets store (is this device's node running?): %w", name, err)
		}
		if v == "" {
			return c, fmt.Errorf("no credentials for %s in the secrets store: run `tailraid secret set %s` and `tailraid secret set %s` first (docs/19)", name, access, secret)
		}
		*f.into = v
	}
	return c, nil
}

// checkObject is written, read back and deleted by add-s3: a key that cannot do all
// three is refused before anything is registered (docs/10 §4.1).
const checkObject = "tailraid-check"

func checkBucket(ctx context.Context, b *s3.Bucket) error {
	body := make([]byte, 64)
	_, _ = rand.Read(body)
	if err := b.Put(ctx, checkObject, body); err != nil {
		return fmt.Errorf("write %s: %w", b.Key(checkObject), err)
	}
	got, err := b.Get(ctx, checkObject, int64(len(body)))
	if err != nil {
		return fmt.Errorf("read %s back: %w", b.Key(checkObject), err)
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("read %s back: the bucket returned other bytes", b.Key(checkObject))
	}
	if err := b.Delete(ctx, checkObject); err != nil {
		return fmt.Errorf("delete %s: %w", b.Key(checkObject), err)
	}
	return nil
}

// addS3Cmd is `node add-s3 <name>`: check the bucket with the credentials the secrets
// store holds for it, then register it through the leader (docs/10 §4.1, docs/19 §6).
func addS3Cmd() *cobra.Command {
	var t s3.Target
	var capacity string
	c := &cobra.Command{
		Use:   "add-s3 <name> --endpoint URL --bucket NAME --capacity SIZE",
		Short: "Add an S3 bucket to the volume as a placement target (its credentials: secret set s3/<name>/access_key and secret_key first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			size, err := config.ParseBytes(capacity)
			if err != nil || size == 0 {
				return fmt.Errorf("--capacity %q: a size such as 1T is required: it is the bucket's placement weight", capacity)
			}
			creds, err := s3Creds(cmd, name)
			if err != nil {
				return err
			}
			b, err := s3.New(t, creds, nil)
			if err != nil {
				return err
			}
			if err := checkBucket(cmd.Context(), b); err != nil {
				return fmt.Errorf("bucket check (nothing was registered): %w", err)
			}
			rep, c, addr, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			id := make([]byte, 8)
			_, _ = rand.Read(id)
			op := rpc.MemberOp{Op: "add_s3", Name: name, Capacity: size, S3: &rpc.S3Spec{
				ID: hex.EncodeToString(id), Endpoint: t.Endpoint, Bucket: t.Bucket, Prefix: t.Prefix, Region: t.Region, PathStyle: t.PathStyle,
			}}
			if err := c.Members(cmd.Context(), leaderOr(rep, addr), op); err != nil {
				return fmt.Errorf("register %s: %w", name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added S3 node %s: bucket %s at %s (weight %s); every device serves it with the credentials in the secrets store\n", name, b.Key(""), t.Endpoint, bytesStr(size))
			return nil
		},
	}
	c.Flags().StringVar(&t.Endpoint, "endpoint", "", "the provider's S3 endpoint, http(s)://host[:port]")
	c.Flags().StringVar(&t.Bucket, "bucket", "", "the bucket")
	c.Flags().StringVar(&t.Prefix, "prefix", "", "a prefix inside the bucket (another cluster may share the bucket under its own)")
	c.Flags().StringVar(&t.Region, "region", "", "the bucket's region (default us-east-1)")
	c.Flags().BoolVar(&t.PathStyle, "path-style", false, "address the bucket as endpoint/bucket (MinIO and some providers) instead of bucket.endpoint")
	c.Flags().StringVar(&capacity, "capacity", "", `the bucket's placement weight and budget ("1T")`)
	addNodeFlags(c)
	return c
}
