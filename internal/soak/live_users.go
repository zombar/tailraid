package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// The users surface of a live run (`tailraid verify --users`, docs/21 §10): the phases'
// people are Ed25519 identities the driver makes and signs with for the node each
// request goes to, as the compose driver's are; the first admin's invite comes from an
// admin this device already holds, else from `user bootstrap` on this device's own node.
// Every user the run makes stays on the cluster, and the first one turns enforcement
// on for every mount, which is why the surface is behind its own flag.

// BootstrapInvite mints the admin invite for name through n: as this device's own admin
// when it holds one (AdminHere), else as `user bootstrap` does, which runs on this device
// alone (it writes the daemon's data directory) and on a voter.
func (e *LiveEnv) BootstrapInvite(ctx context.Context, n NodeRef, name string) (string, error) {
	if e.Client == nil {
		return "", errors.New("live: no rpc client")
	}
	var fromAdmin error
	if e.AdminHere {
		raw, err := e.Client.Admin(ctx, n.RPC, rpc.AdminOp{Op: "user_invite", User: name, Admin: true})
		if err == nil {
			return inviteCode(raw)
		}
		fromAdmin = fmt.Errorf("an admin invite by this device's identity: %w", err)
	}
	if n.RPC != e.SelfAddr {
		return "", errors.Join(fromAdmin, fmt.Errorf("live: user bootstrap runs on this device's own node, not %s", n.Name))
	}
	tok, err := users.WriteBootstrap(e.DataDir)
	if err != nil {
		return "", errors.Join(fromAdmin, fmt.Errorf("live: user bootstrap writes the daemon's data directory (run verify --users as the daemon's user, or as an admin of this device): %w", err))
	}
	raw, err := e.Client.Admin(ctx, n.RPC, rpc.AdminOp{Op: "user_bootstrap", Value: tok, User: name})
	if err != nil {
		return "", errors.Join(fromAdmin, err)
	}
	return inviteCode(raw)
}

// inviteCode is the code of an invite reply.
func inviteCode(raw json.RawMessage) (string, error) {
	var rep struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Code == "" {
		return "", fmt.Errorf("live: invite reply %q: %w", raw, err)
	}
	return rep.Code, nil
}

// SignedOp sends an admin op to n signed by id for n's own node ID (I-344), or unsigned
// when id is nil; the device's own signer is left out either way.
func (e *LiveEnv) SignedOp(ctx context.Context, n NodeRef, id *users.Identity, op rpc.AdminOp) (json.RawMessage, error) {
	if e.Client == nil {
		return nil, errors.New("live: no rpc client")
	}
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	sig := ""
	if id != nil {
		nodeID, ok := e.ids[n.RPC]
		if !ok {
			if _, err := e.Nodes(ctx); err != nil {
				return nil, err
			}
			if nodeID, ok = e.ids[n.RPC]; !ok {
				return nil, fmt.Errorf("live: %s (%s) is not in this device's status", n.Name, n.RPC)
			}
		}
		nonce, err := users.NewNonce()
		if err != nil {
			return nil, err
		}
		sig = users.Sign(*id, e.Cluster, nodeID, http.MethodPost, "/v1/admin", body, time.Now().Unix(), nonce).String()
	}
	return e.Client.AdminSigned(ctx, n.RPC, body, sig)
}

// MountCredential is the binding key this device's mount presents: the uid running
// verify, or the SMB account on Windows.
func (e *LiveEnv) MountCredential() string { return e.MountCred }

var _ UsersEnv = (*LiveEnv)(nil)
