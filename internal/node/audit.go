package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zombar/tailraid/internal/audit"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// The node's side of the audit log (docs/21 §8): the leader records applied operations
// from the FSM's apply hook, every node its admin operations, sign-ins and refusals, the
// loop flushes and the leader retains, and `audit ls` reads it back.

// AuditFlushEvery is how often the loop writes what changed and closes refusal windows.
const AuditFlushEvery = 30 * time.Second

// audited is the FSM's apply hook: on the leader, every applied users or secrets op is a
// record; a follower records nothing, so there is one copy (I-357).
func (a *adminOps) audited(cmd meta.Command, index uint64, res meta.Result) {
	if a.audit == nil || !a.node.IsLeader() {
		return
	}
	if rec, ok := audit.Applied(cmd, index, res, a.node.FSM); ok {
		a.audit.Record(rec)
	}
}

// auditOp records an admin op that needed admins, a personal secret read and every
// refusal of the needs table; nothing before the cluster has users.
func (a *adminOps) auditOp(op rpc.AdminOp, err error) {
	if a.audit == nil || !a.node.FSM.Enforcing() {
		return
	}
	need, _ := op.Needs()
	actor := a.userName(op.Actor)
	switch {
	case err != nil && (need != rpc.NeedNone):
		if isRefusal(err) {
			a.audit.Refuse(actor, "", audit.KindOp+op.Op, opTargetName(op), err.Error())
		}
	case op.Op == "secret_get":
		a.audit.Record(audit.Record{Actor: actor, Kind: audit.KindSecretGet, Target: op.Name})
	case need == rpc.NeedAdmins:
		a.audit.Record(audit.Record{Actor: actor, Kind: audit.KindOp + op.Op, Target: opTargetName(op)})
	}
}

func isRefusal(err error) bool {
	return errors.Is(err, meta.ErrAccess) || errors.Is(err, meta.ErrInvite) || errors.Is(err, rpc.ErrUnauthorized)
}

func opTargetName(op rpc.AdminOp) string {
	for _, s := range []string{op.Path, op.Name, op.User, op.Group} {
		if s != "" {
			return s
		}
	}
	return ""
}

// userName is a user's name, "" for nobody.
func (a *adminOps) userName(idx meta.UserIdx) string {
	if idx == 0 {
		return ""
	}
	if u, ok := a.node.FSM.User(idx); ok {
		return u.Name
	}
	return fmt.Sprintf("user#%d", idx)
}

// auditLoop flushes the log every AuditFlushEvery and, on the leader, drops the segments
// past audit.retention once an hour.
func (a *adminOps) auditLoop(ctx context.Context) {
	if a.audit == nil {
		return
	}
	t := a.clk.NewTimer(AuditFlushEvery)
	defer t.Stop()
	lastRetain := a.clk.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			if a.healthy() {
				if err := a.audit.Flush(ctx, false); err != nil {
					a.log.Warn("audit: flush", "err", err)
				}
				if now := a.clk.Now(); a.node.IsLeader() && now.Sub(lastRetain) >= time.Hour {
					lastRetain = now
					if n, err := audit.Retain(ctx, a.audit.Vol, a.auditKeep, now); err != nil {
						a.log.Warn("audit: retention", "err", err)
					} else if n > 0 {
						a.log.Info("audit: segments dropped past retention", "count", n)
					}
				}
			}
			t.Reset(AuditFlushEvery)
		}
	}
}

// sealAudit writes what the log holds at a stop, refusal windows included.
func (a *adminOps) sealAudit() {
	if a.audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), StatsSealTimeout)
	defer cancel()
	if err := a.audit.Flush(ctx, true); err != nil {
		a.log.Warn("audit: the stop's flush did not reach the volume", "err", err)
	}
}

// webAudit is the web UI's audit hook.
type webAudit struct{ w *audit.Writer }

func (h webAudit) SignedIn(user string) {
	h.w.Record(audit.Record{Actor: user, Kind: audit.KindSignIn})
}

func (h webAudit) Refused(user, kind, route, detail string) {
	h.w.Refuse(user, "", audit.KindWeb, route, detail)
}

// auditList answers `audit ls`: a user's own records, everyone's for an admin (I-358).
func (a *adminOps) auditList(ctx context.Context, op rpc.AdminOp) (any, error) {
	if a.audit == nil {
		return nil, fmt.Errorf("audit: no log on this node")
	}
	f := audit.Filter{User: op.User, Kind: op.Kind, Limit: op.Limit}
	if op.Since != "" {
		since, err := time.Parse(time.RFC3339, op.Since)
		if err != nil {
			return nil, fmt.Errorf("audit: since %q: %w", op.Since, meta.ErrBadCommand)
		}
		f.Since = since
	}
	if a.node.FSM.Enforcing() && !a.node.FSM.IsAdmin(op.Actor) {
		f.User = a.userName(op.Actor) // their own records and no one else's
	}
	if err := a.audit.Flush(ctx, false); err != nil {
		a.log.Warn("audit: flush before a listing", "err", err)
	}
	recs, err := audit.List(ctx, a.audit.Vol, f)
	if err != nil {
		return nil, err
	}
	if recs == nil {
		recs = []audit.Record{}
	}
	return recs, nil
}

// userWarnings are check's users warnings (I-358): dormant users, and this device's
// bindings to users that no longer exist.
func (a *adminOps) userWarnings(ctx context.Context) []string {
	st := a.node.FSM
	if !st.Enforcing() {
		return nil
	}
	var out []string
	if a.audit != nil {
		if seen, err := audit.Seen(ctx, a.audit.Vol); err == nil {
			for _, u := range audit.Dormant(st.Users(), seen, a.dormant, a.clk.Now()) {
				out = append(out, fmt.Sprintf("user %s has not signed in or signed an operation in %s: review the account (docs/21 §8)", u, a.dormant))
			}
		}
	}
	if a.bindings != nil {
		for _, b := range a.bindings.List() {
			if _, ok := st.UserByName(b.User); !ok {
				out = append(out, fmt.Sprintf("this device binds %s to %q, a user that does not exist: `tailraid user unbind` it (docs/21 §5)", b.Credential, b.User))
			}
		}
	}
	return out
}
