package soak

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/secrets"
)

// --- 24 secrets ------------------------------------------------------------------------
//
// docs/19 on a running cluster. A secret made for this run is set through one node and
// read back through every other: the sealed value replicates and each node opens it with
// the cluster key, fetching the key from a member when its keychain has none (a). Every
// node lists it, readable, with no value in the reply (b). A replace through another
// node reads back through the first (c). A node that lost its keychain copy (a
// reinstall: the copy wiped and the node restarted) fetches the key and reads it (d).
// With the node that set it killed, the others still read it (e). A removal through one
// node reaches every node (f). The secret is removed after, whatever happened; nothing
// else in the store is touched, so a live cluster's own secrets stay.

// ErrUnsupported is an environment that cannot do what a check asks: the check is
// recorded as skipped.
var ErrUnsupported = errors.New("soak: not supported by this environment")

// SecretEnv is the secrets store surface a phase needs: the CLI's admin ops through one
// node, and wiping one node's keychain copy of the cluster key (then restarting it).
type SecretEnv interface {
	SecretSet(ctx context.Context, n NodeRef, name, value string) error
	SecretGet(ctx context.Context, n NodeRef, name string) (string, error)
	SecretList(ctx context.Context, n NodeRef) (json.RawMessage, error)
	SecretRemove(ctx context.Context, n NodeRef, name string) error
	// ForgetSecretKey wipes the node's own copy of the cluster key and restarts it, so its
	// next read must fetch the key from a member; ErrUnsupported where that is not ours to
	// do (a live cluster's devices).
	ForgetSecretKey(ctx context.Context, n NodeRef) error
}

func (a *Admin) secretOp(ctx context.Context, n NodeRef, op rpc.AdminOp) (json.RawMessage, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = a.call(ctx, n.RPC, "POST", "/v1/admin", body, &raw)
	return raw, err
}

// SecretSet stores a secret through n (`tailraid secret set`).
func (a *Admin) SecretSet(ctx context.Context, n NodeRef, name, value string) error {
	_, err := a.secretOp(ctx, n, rpc.AdminOp{Op: "secret_set", Name: name, Value: value})
	return err
}

// SecretGet opens a secret through n.
func (a *Admin) SecretGet(ctx context.Context, n NodeRef, name string) (string, error) {
	raw, err := a.secretOp(ctx, n, rpc.AdminOp{Op: "secret_get", Name: name})
	if err != nil {
		return "", err
	}
	var v struct {
		Value string `json:"value"`
	}
	err = json.Unmarshal(raw, &v)
	return v.Value, err
}

// SecretList is n's `tailraid secret ls` reply, as sent.
func (a *Admin) SecretList(ctx context.Context, n NodeRef) (json.RawMessage, error) {
	return a.secretOp(ctx, n, rpc.AdminOp{Op: "secret_ls"})
}

// SecretRemove removes a secret through n.
func (a *Admin) SecretRemove(ctx context.Context, n NodeRef, name string) error {
	_, err := a.secretOp(ctx, n, rpc.AdminOp{Op: "secret_rm", Name: name})
	return err
}

// ForgetSecretKey is not ours to do on a live cluster's devices.
func (a *Admin) ForgetSecretKey(context.Context, NodeRef) error { return ErrUnsupported }

// secretsDeadline bounds the wait for a write to reach a node.
const secretsDeadline = 30 * time.Second

// waitSecret polls n until get answers want, or the deadline.
func waitSecret(ctx context.Context, r *Run, se SecretEnv, n NodeRef, name, want string) (string, error) {
	var got string
	var err error
	for start := time.Now(); ; {
		got, err = se.SecretGet(ctx, n, name)
		if err == nil && got == want || time.Since(start) > secretsDeadline {
			return got, err
		}
		if werr := r.Env.Wait(ctx, 250*time.Millisecond); werr != nil {
			return got, werr
		}
	}
}

func phaseSecrets(ctx context.Context, r *Run) error {
	se, ok := r.Env.(SecretEnv)
	if !ok {
		r.skip("the environment has no secrets surface")
		return nil
	}
	nodes, err := r.Env.Nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) < 2 {
		r.skip("one node: nothing to replicate to")
		return nil
	}
	name := fmt.Sprintf("soak/%016x", r.Rng.Uint64())
	value := hex.EncodeToString(r.randomBytes(24))
	u := r.undoer(ctx)
	defer u.run()
	u.add("remove "+name, func(c context.Context) error {
		err := se.SecretRemove(c, nodes[0], name)
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})

	// (a) set through one node, read through every node
	if err := se.SecretSet(ctx, nodes[0], name, value); err != nil {
		r.check("I-304, I-306", "secret set through "+nodes[0].Name, false, err.Error())
		return nil
	}
	for _, n := range nodes {
		got, err := waitSecret(ctx, r, se, n, name, value)
		r.check("I-303, I-304, I-306", "secret replicated to "+n.Name, err == nil && got == value, secretWhy(err, got != value, "read another value"))
	}

	// (b) every node lists it, readable, and no reply carries the value
	for _, n := range nodes {
		raw, err := se.SecretList(ctx, n)
		var es []secrets.Entry
		if err == nil {
			err = json.Unmarshal(raw, &es)
		}
		listed := false
		for _, e := range es {
			listed = listed || e.Name == name && e.Readable
		}
		leak := strings.Contains(string(raw), value)
		r.check("I-306, I-307", "secret listed on "+n.Name+" without its value", err == nil && listed && !leak, secretWhy(err, !listed || leak, "not listed readable, or the reply carries the value"))
	}

	// (c) a replace through another node reads back through the first
	last := nodes[len(nodes)-1]
	value2 := hex.EncodeToString(r.randomBytes(24))
	err = se.SecretSet(ctx, last, name, value2)
	got, gerr := "", err
	if err == nil {
		got, gerr = waitSecret(ctx, r, se, nodes[0], name, value2)
	}
	r.check("I-306", "secret replaced through "+last.Name+" reads back on "+nodes[0].Name, gerr == nil && got == value2, secretWhy(gerr, got != value2, "read the old value"))

	// (d) a node that lost its keychain copy fetches the key
	switch err := se.ForgetSecretKey(ctx, last); {
	case errors.Is(err, ErrUnsupported):
		r.checkSkipped("I-304", "a node without its key fetches it", "the environment cannot wipe a device's keychain")
	case err != nil:
		r.check("I-304", "a node without its key fetches it", false, "forget: "+err.Error())
	default:
		got, err := waitSecret(ctx, r, se, last, name, value2)
		r.check("I-303, I-304", "a node without its key fetches it", err == nil && got == value2, secretWhy(err, got != value2, "read another value"))
	}

	// (e) the others read it with the node that set it down
	if r.Env.Caps().Kill && len(nodes) >= 3 {
		u.add("start "+nodes[0].Name, func(c context.Context) error { return r.Env.Start(c, nodes[0]) })
		if err := r.Env.Kill(ctx, nodes[0]); err != nil {
			r.check("I-303", "secret read with its setter down", false, "kill: "+err.Error())
		} else {
			got, err := waitSecret(ctx, r, se, nodes[1], name, value2)
			r.check("I-303", "secret read with its setter down", err == nil && got == value2, secretWhy(err, got != value2, "read another value"))
			if err := r.Env.Start(ctx, nodes[0]); err != nil {
				return err
			}
			u.done("start " + nodes[0].Name)
		}
	} else {
		r.checkSkipped("I-303", "secret read with its setter down", "needs Kill and three nodes")
	}

	// (f) a removal through one node reaches every node
	err = se.SecretRemove(ctx, nodes[1], name)
	r.check("I-307", "secret removed through "+nodes[1].Name, err == nil, secretWhy(err, false, ""))
	for _, n := range nodes {
		gone := false
		for start := time.Now(); time.Since(start) < secretsDeadline; {
			if _, err := se.SecretGet(ctx, n, name); err != nil && isNotFound(err) {
				gone = true
				break
			}
			if werr := r.Env.Wait(ctx, 250*time.Millisecond); werr != nil {
				return werr
			}
		}
		r.check("I-307", "secret removal reached "+n.Name, gone, "still answered")
	}
	u.done("remove " + name)
	return nil
}

// isNotFound reports an admin reply that the secret does not exist.
func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, secrets.ErrNotFound) || strings.Contains(err.Error(), "no such secret"))
}

func secretWhy(err error, bad bool, why string) string {
	switch {
	case err != nil:
		return err.Error()
	case bad:
		return why
	}
	return ""
}
