// SPDX-License-Identifier: BSD-3-Clause

package smb

import "slices"

// Who may use a share, and for what.
//
// Until this existed, every authenticated user got every share with full
// access: a server with two people on it could not keep one out of the
// other's images. That is a surprise rather than a missing feature, which is
// why it is a refusal now and not a note in the documentation.
//
// Three rules, and they compose:
//
//	allow    empty  -> anyone who authenticated may connect
//	allow    listed -> only those named may connect, everyone else is refused
//	writers  empty  -> whoever may connect may write, unless ReadOnly
//	writers  listed -> only those named may write; the rest get a read-only
//	                   share, told to them in the access mask at connect time
//	ReadOnly        -> nobody writes, whatever writers says
//
// The mask matters as much as the refusal: a client told read-only greys the
// actions out, and a person sees why. A client told it may write, whose writes
// are then refused one at a time, sees a broken share.

// AllowUsers names the only users who may connect to this share. Called more
// than once, the names accumulate.
func AllowUsers(users ...string) ShareOption {
	return func(s *share) { s.allow = append(s.allow, users...) }
}

// WriteUsers names the only users who may write to this share. Everyone else
// who may connect gets it read-only.
func WriteUsers(users ...string) ShareOption {
	return func(s *share) { s.writers = append(s.writers, users...) }
}

// Access hands the decision to the caller: decide is asked, when somebody
// connects, whether that user may connect and whether they may write. It
// replaces [AllowUsers] and [WriteUsers] for this share, and [ReadOnly]
// still wins over it.
//
// It exists for a caller whose people change while the server runs -- a
// directory re-read every few minutes, people whose access follows groups
// an identity provider asserts -- and for whom a list given once, at
// Share, is a list that goes stale. The answer is asked at every TREE
// CONNECT and every share listing, so a change reaches the next
// connection; one already open keeps what it was given, as a mount does.
func Access(decide func(user string) (connect, write bool)) ShareOption {
	return func(s *share) { s.decide = decide }
}

// mayConnect reports whether a user may use this share at all.
func (s *share) mayConnect(user string) bool {
	if s.decide != nil {
		ok, _ := s.decide(user)
		return ok
	}
	return len(s.allow) == 0 || slices.Contains(s.allow, user)
}

// readOnlyFor reports whether this user's connection to the share is
// read-only, which is the share's own answer plus this user's.
func (s *share) readOnlyFor(user string) bool {
	if s.ro {
		return true
	}
	if s.decide != nil {
		_, write := s.decide(user)
		return !write
	}
	return len(s.writers) > 0 && !slices.Contains(s.writers, user)
}

// A treeConn is one share, as connected by one user: the share plus what that
// person may do with it.
//
// It is what a tree id resolves to, rather than the share itself, because the
// answer to "may this write" belongs to the CONNECTION. Two people can hold
// the same share at once with different answers.
type treeConn struct {
	sh *share
	ro bool
}
