package meta

import "errors"

// Sentinel errors returned in Result.Err. Each has a stable numeric code so a Result
// survives encoding identically on every node (I-26).
var (
	ErrExist           = errors.New("meta: entry exists")
	ErrNotExist        = errors.New("meta: no such entry")
	ErrClosed          = errors.New("meta: fsm closed")
	ErrNotEmpty        = errors.New("meta: directory not empty")
	ErrIsDir           = errors.New("meta: is a directory")
	ErrNotDir          = errors.New("meta: not a directory")
	ErrLoop            = errors.New("meta: rename would create a loop")
	ErrConflict        = errors.New("meta: version conflict")
	ErrUnknownChunk    = errors.New("meta: unknown chunk")
	ErrNeedTailRewrite = errors.New("meta: truncate requires rewriting the tail chunk")
	ErrBadCommand      = errors.New("meta: malformed command")
	ErrNoSuchNode      = errors.New("meta: no such node")
	ErrNodeNotEmpty    = errors.New("meta: node still holds sole shards")
	ErrPolicy          = errors.New("meta: invalid volume policy")
	ErrNameTooLong     = errors.New("meta: name longer than MaxNameLen bytes")
	ErrProtected       = errors.New("meta: entry is under a protected directory")
	ErrInTrash         = errors.New("meta: entry is in the trash")
	ErrCodec           = errors.New("meta: a member runs a codec too old for this command")
	// ErrSuperseded refuses a Commit that continues a rewrite another rewrite of the
	// file started after (docs/04 §6, I-223); the flusher drops the inode's dirty data.
	ErrSuperseded = errors.New("meta: another rewrite of the file started later")
)

// ErrUnknownOutcome is returned by a Proposer (never in Result.Err, so it has no wire
// code) when a command was handed to Raft but its fate is unknown: leadership was lost,
// the forward timed out, or the leader's reply was lost (docs/02 §forwarding, I-95).
// The caller must not assume either outcome; it re-proposes and reads the answer, or
// re-checks the state.
var ErrUnknownOutcome = errors.New("meta: proposal outcome unknown")

var errByCode = []error{
	nil, // 0
	ErrExist, ErrNotExist, ErrNotEmpty, ErrIsDir, ErrNotDir, ErrLoop, ErrConflict,
	ErrUnknownChunk, ErrNeedTailRewrite, ErrBadCommand, ErrNoSuchNode, ErrNodeNotEmpty, ErrPolicy,
	ErrNameTooLong, ErrProtected, ErrInTrash, ErrCodec, ErrSuperseded, // appended; codes are never renumbered
}

// ErrorCode maps a sentinel error to its wire code. nil is 0; a non-sentinel error is
// also 0 (callers must only put sentinels in Result.Err).
func ErrorCode(err error) uint8 {
	if err == nil {
		return 0
	}
	for i := 1; i < len(errByCode); i++ {
		if errors.Is(err, errByCode[i]) {
			return uint8(i)
		}
	}
	return 0
}

// ErrorFromCode is the inverse of ErrorCode. ok is false for an unknown code.
func ErrorFromCode(code uint8) (err error, ok bool) {
	if int(code) >= len(errByCode) {
		return nil, false
	}
	return errByCode[code], true
}
