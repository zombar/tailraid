package audit

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
)

// Names resolves what a record names: the FSM's read API.
type Names interface {
	User(idx meta.UserIdx) (meta.User, bool)
	Group(idx meta.GroupIdx) (meta.Group, bool)
	PathOf(ino meta.Ino) ([]string, error)
}

// Kinds of the applied users and secrets operations (I-357), and of the records the
// nodes write themselves.
const (
	KindInvite        = "invite"
	KindInviteDelete  = "invite.delete"
	KindUserCreate    = "user.create"
	KindUserKeyAdd    = "user.key_add"
	KindUserKeyRemove = "user.key_rm"
	KindUserDisable   = "user.disable"
	KindUserEnable    = "user.enable"
	KindUserRemove    = "user.rm"
	KindGroupCreate   = "group.create"
	KindGroupRemove   = "group.rm"
	KindGroupAdd      = "group.add"
	KindGroupDrop     = "group.remove"
	KindGrant         = "grant"
	KindRevoke        = "revoke"
	KindSessionMint   = "session.mint"
	KindSessionEnd    = "session.end"
	KindSecretSet     = "secret.set"
	KindSecretRemove  = "secret.rm"
	KindSecretGet     = "secret.get"
	KindSignIn        = "session.signin"
	KindOp            = "op."  // an admin operation that needed admins: op.<name>
	KindWeb           = "web"  // a browser request refused
	KindMount         = "file" // a file operation refused on a gateway
	KindRPC           = "rpc"  // a signed request refused by the RPC server
)

// Applied is the record of an applied command (ops 23–25 and 28–32), or false for a
// command the log does not carry. A refused command is a refused record.
func Applied(cmd meta.Command, index uint64, res meta.Result, names Names) (Record, bool) {
	rec := Record{Index: index, Result: ResultOK}
	if res.Err != nil {
		rec.Result, rec.Detail = ResultRefused, res.Err.Error()
	}
	if cmd.Actor != 0 {
		rec.Actor = userName(names, cmd.Actor)
	}
	switch cmd.Op {
	case meta.OpInvite:
		p := cmd.InviteOp
		if p == nil {
			return rec, false
		}
		rec.Kind = KindInvite
		if p.Delete {
			rec.Kind = KindInviteDelete
		}
		switch p.Invite.Kind {
		case meta.InviteKey:
			rec.Target = userName(names, p.Invite.ForUser)
			rec.Detail = joinDetail(rec.Detail, "a key for another device")
		default:
			rec.Target = p.Invite.Name
			if p.Invite.Admin {
				rec.Detail = joinDetail(rec.Detail, "admin")
			}
		}
		if p.Bootstrap {
			rec.Detail = joinDetail(rec.Detail, "bootstrap")
		}
	case meta.OpUser:
		p := cmd.UserOp
		if p == nil {
			return rec, false
		}
		switch p.Action {
		case meta.UserCreate:
			rec.Kind, rec.Target = KindUserCreate, p.Name
			if rec.Result == ResultOK && res.Ino != 0 {
				rec.Actor = p.Name // the person themselves: the invite was theirs to use
			}
		case meta.UserKeyAdd:
			rec.Kind, rec.Target = KindUserKeyAdd, userName(names, p.Target)
			if rec.Target == "" {
				rec.Target = userName(names, meta.UserIdx(res.Ino)) //nolint:gosec // a user index rides in Ino
			}
		case meta.UserKeyRemove:
			rec.Kind, rec.Target, rec.Key = KindUserKeyRemove, userName(names, p.Target), hex.EncodeToString(p.KeyID[:])
		case meta.UserDisable:
			rec.Kind, rec.Target = KindUserDisable, userName(names, p.Target)
		case meta.UserEnable:
			rec.Kind, rec.Target = KindUserEnable, userName(names, p.Target)
		case meta.UserRemove:
			rec.Kind, rec.Target = KindUserRemove, userName(names, p.Target)
		default:
			return rec, false
		}
	case meta.OpGroup:
		p := cmd.GroupOp
		if p == nil {
			return rec, false
		}
		switch p.Action {
		case meta.GroupCreate:
			rec.Kind, rec.Target = KindGroupCreate, "@"+p.Name
		case meta.GroupRemove:
			rec.Kind, rec.Target = KindGroupRemove, "@"+groupName(names, p.Group)
		case meta.GroupAddMember:
			rec.Kind, rec.Target = KindGroupAdd, "@"+groupName(names, p.Group)
			rec.Detail = joinDetail(rec.Detail, userName(names, p.User))
		case meta.GroupRemoveMember:
			rec.Kind, rec.Target = KindGroupDrop, "@"+groupName(names, p.Group)
			rec.Detail = joinDetail(rec.Detail, userName(names, p.User))
		default:
			return rec, false
		}
	case meta.OpGrant:
		p := cmd.GrantOp
		if p == nil {
			return rec, false
		}
		rec.Kind = KindGrant
		if p.Role == meta.RoleNone {
			rec.Kind = KindRevoke
		}
		rec.Target = pathName(names, p.Ino)
		subject := userName(names, meta.UserIdx(p.Subject.Idx))
		if p.Subject.Kind == meta.SubjectGroup {
			subject = "@" + groupName(names, meta.GroupIdx(p.Subject.Idx))
		}
		rec.Detail = joinDetail(rec.Detail, subject+" "+p.Role.String())
	case meta.OpSession:
		p := cmd.SessionOp
		if p == nil {
			return rec, false
		}
		rec.Kind = KindSessionMint
		if p.Delete {
			rec.Kind = KindSessionEnd
			if p.All {
				rec.Detail = joinDetail(rec.Detail, "every session of "+userName(names, p.User))
			}
		} else {
			rec.Key = hex.EncodeToString(p.Key[:])
		}
	case meta.OpSecretPut:
		p := cmd.SecretPut
		if p == nil {
			return rec, false
		}
		rec.Kind, rec.Target = KindSecretSet, p.Secret.Name
		if rec.Actor == "" && p.Secret.ByUser != 0 {
			rec.Actor = userName(names, p.Secret.ByUser)
		}
	case meta.OpSecretDelete:
		p := cmd.SecretDelete
		if p == nil {
			return rec, false
		}
		rec.Kind, rec.Target = KindSecretRemove, p.Name
		if p.All {
			rec.Target, rec.Detail = "*", joinDetail(rec.Detail, "every secret and the key")
		}
	default:
		return rec, false
	}
	return rec, true
}

func userName(names Names, idx meta.UserIdx) string {
	if idx == 0 || names == nil {
		return ""
	}
	if u, ok := names.User(idx); ok {
		return u.Name
	}
	return fmt.Sprintf("user#%d", idx)
}

func groupName(names Names, idx meta.GroupIdx) string {
	if names == nil {
		return fmt.Sprintf("group#%d", idx)
	}
	if g, ok := names.Group(idx); ok {
		return g.Name
	}
	return fmt.Sprintf("group#%d", idx)
}

func pathName(names Names, ino meta.Ino) string {
	if names == nil {
		return fmt.Sprintf("ino %d", ino)
	}
	parts, err := names.PathOf(ino)
	if err != nil {
		return fmt.Sprintf("ino %d", ino)
	}
	return "/" + strings.Join(parts, "/")
}

func joinDetail(have, more string) string {
	if have == "" {
		return more
	}
	if more == "" {
		return have
	}
	return have + "; " + more
}
