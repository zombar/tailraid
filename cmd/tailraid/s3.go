package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// SecretKeyEnv carries an S3 secret key to `node add-s3` and `node keys`; without it the
// key is read from one line of stdin. It is never a flag: it would sit in the shell
// history (docs/10 §4.1).
const SecretKeyEnv = "TAILRAID_S3_SECRET_KEY" //nolint:gosec // the name of a variable, not a credential

// readCreds pairs the access key with the secret from the environment or stdin.
func readCreds(cmd *cobra.Command, access string) (s3.Credentials, error) {
	if access == "" {
		return s3.Credentials{}, errors.New("--access-key is required")
	}
	secret := os.Getenv(SecretKeyEnv)
	if secret == "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "secret key for %s: ", access)
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && line == "" {
			return s3.Credentials{}, fmt.Errorf("no secret key (set %s or type it on stdin): %w", SecretKeyEnv, err)
		}
		secret = strings.TrimSpace(line)
	}
	if secret == "" {
		return s3.Credentials{}, fmt.Errorf("empty secret key (set %s or type it on stdin)", SecretKeyEnv)
	}
	return s3.Credentials{AccessKey: access, SecretKey: secret}, nil
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

// addS3Cmd is `node add-s3 <name>`: check the bucket with this device's key, register it
// through the leader, then keep the key on this device (docs/10 §4.1).
func addS3Cmd() *cobra.Command {
	var t s3.Target
	var capacity, access string
	c := &cobra.Command{
		Use:   "add-s3 <name> --endpoint URL --bucket NAME --capacity SIZE --access-key KEY",
		Short: "Add an S3 bucket to the volume as a placement target (the secret key comes from " + SecretKeyEnv + " or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			size, err := config.ParseBytes(capacity)
			if err != nil || size == 0 {
				return fmt.Errorf("--capacity %q: a size such as 1T is required: it is the bucket's placement weight", capacity)
			}
			creds, err := readCreds(cmd, access)
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
			if err := cluster.SaveS3Key(cfg.Node.DataDir, name, creds); err != nil {
				return fmt.Errorf("registered %s, but its key was not saved on this device (run `tailraid node keys %s`): %w", name, name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added S3 node %s: bucket %s at %s (weight %s)\n", name, b.Key(""), t.Endpoint, bytesStr(size))
			fmt.Fprintf(cmd.OutOrStdout(), "give every other device that writes the key: tailraid node keys %s --access-key %s\n", name, access)
			return nil
		},
	}
	c.Flags().StringVar(&t.Endpoint, "endpoint", "", "the provider's S3 endpoint, http(s)://host[:port]")
	c.Flags().StringVar(&t.Bucket, "bucket", "", "the bucket")
	c.Flags().StringVar(&t.Prefix, "prefix", "", "a prefix inside the bucket (another cluster may share the bucket under its own)")
	c.Flags().StringVar(&t.Region, "region", "", "the bucket's region (default us-east-1)")
	c.Flags().BoolVar(&t.PathStyle, "path-style", false, "address the bucket as endpoint/bucket (MinIO and some providers) instead of bucket.endpoint")
	c.Flags().StringVar(&capacity, "capacity", "", `the bucket's placement weight and budget ("1T")`)
	c.Flags().StringVar(&access, "access-key", "", "the access key ID")
	addNodeFlags(c)
	return c
}

// keysCmd is `node keys <name>`: keep an S3 node's key on this device, after checking it
// against the bucket (docs/10 §4.1).
func keysCmd() *cobra.Command {
	var access string
	c := &cobra.Command{
		Use:   "keys <s3-node> --access-key KEY",
		Short: "Give this device the key of an S3 node (the secret key comes from " + SecretKeyEnv + " or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			rep, _, _, err := fetchReport(cmd.Context(), cmd)
			if err != nil {
				return err
			}
			n, err := findNode(rep, args[0])
			if err != nil {
				return err
			}
			if n.S3 == nil {
				return fmt.Errorf("%s is a %s, not an S3 node", n.Name, n.Kind)
			}
			creds, err := readCreds(cmd, access)
			if err != nil {
				return err
			}
			b, err := s3.New(s3.Target{Endpoint: n.S3.Endpoint, Bucket: n.S3.Bucket, Prefix: n.S3.Prefix, Region: n.S3.Region, PathStyle: n.S3.PathStyle}, creds, nil)
			if err != nil {
				return err
			}
			if err := b.Probe(cmd.Context()); err != nil {
				return fmt.Errorf("the key does not open bucket %s at %s (nothing was saved): %w", n.S3.Bucket, n.S3.Endpoint, err)
			}
			if err := cluster.SaveS3Key(cfg.Node.DataDir, n.Name, creds); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved the key of %s; the running node uses it from its next probe\n", n.Name)
			return nil
		},
	}
	c.Flags().StringVar(&access, "access-key", "", "the access key ID")
	addNodeFlags(c)
	return c
}
