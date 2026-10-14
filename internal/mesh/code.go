package mesh

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// The join code (docs/20 §4): version ‖ cluster ID ‖ token ‖ bootstrap key ‖ bootstrap
// host ‖ mesh port ‖ RPC port ‖ endpoints ‖ check, as one lowercase base32 string.
const (
	codeVersion  = 1
	TokenLen     = 16 // base32 characters: 80 bits
	MaxEndpoints = 8
	checkLen     = 4
	maxEndpoint  = 255 // one length byte per endpoint
)

// Errors of the join code.
var (
	ErrBadCode  = errors.New("mesh: bad join code")
	ErrBadToken = errors.New("mesh: bad token")
)

var (
	b32      = base32.StdEncoding.WithPadding(base32.NoPadding)
	tokenRE  = regexp.MustCompile(`^[a-z2-7]{16}$`)
	codeTail = len(Key{}) + 1 + 2 + 2 + 1 + checkLen // after the token: key, host, mesh port, RPC port, count, check
)

// JoinCode is what `init` and `tailraid token` print and `join <code>` takes.
type JoinCode struct {
	ClusterID [16]byte
	Token     string
	Key       Key      // the bootstrap device's public key
	Host      uint8    // its host number in the subnet
	Port      uint16   // its mesh port
	RPCPort   uint16   // its RPC port: where /v1/join goes once the tunnel is up
	Endpoints []string // where to send the introduce: host:port, at most MaxEndpoints
}

// NewToken returns a fresh join token: TokenLen random base32 characters.
func NewToken() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mesh: crypto/rand: " + err.Error())
	}
	return strings.ToLower(b32.EncodeToString(b[:]))
}

// ValidToken reports whether s is a token of the mesh's form.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

// Encode renders the code.
func (c JoinCode) Encode() (string, error) {
	if !ValidToken(c.Token) {
		return "", fmt.Errorf("%w: %q", ErrBadToken, c.Token)
	}
	if len(c.Endpoints) > MaxEndpoints {
		return "", fmt.Errorf("%w: %d endpoints, at most %d", ErrBadCode, len(c.Endpoints), MaxEndpoints)
	}
	if c.Host < FirstHost || c.Host > MaxHost {
		return "", fmt.Errorf("%w: host %d", ErrBadCode, c.Host)
	}
	b := make([]byte, 0, 1+16+TokenLen+codeTail+len(c.Endpoints)*16)
	b = append(b, codeVersion)
	b = append(b, c.ClusterID[:]...)
	b = append(b, c.Token...)
	b = append(b, c.Key[:]...)
	b = append(b, c.Host)
	b = binary.BigEndian.AppendUint16(b, c.Port)
	b = binary.BigEndian.AppendUint16(b, c.RPCPort)
	b = append(b, byte(len(c.Endpoints)))
	for _, e := range c.Endpoints {
		if e == "" || len(e) > maxEndpoint {
			return "", fmt.Errorf("%w: endpoint %q", ErrBadCode, e)
		}
		if err := checkEndpoint(e); err != nil {
			return "", err
		}
		b = append(b, byte(len(e)))
		b = append(b, e...)
	}
	sum := sha256.Sum256(b)
	b = append(b, sum[:checkLen]...)
	return strings.ToLower(b32.EncodeToString(b)), nil
}

// DecodeJoinCode reads a code; whitespace and case are forgiven.
func DecodeJoinCode(s string) (JoinCode, error) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	b, err := b32.DecodeString(s)
	if err != nil {
		return JoinCode{}, fmt.Errorf("%w: %w", ErrBadCode, err)
	}
	if len(b) < 1+16+TokenLen+codeTail {
		return JoinCode{}, fmt.Errorf("%w: too short", ErrBadCode)
	}
	body, check := b[:len(b)-checkLen], b[len(b)-checkLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:checkLen], check) {
		return JoinCode{}, fmt.Errorf("%w: a character is wrong", ErrBadCode)
	}
	if body[0] != codeVersion {
		return JoinCode{}, fmt.Errorf("%w: version %d (this tailraid reads %d)", ErrBadCode, body[0], codeVersion)
	}
	var c JoinCode
	p := 1
	copy(c.ClusterID[:], body[p:p+16])
	p += 16
	c.Token = strings.ToLower(string(body[p : p+TokenLen]))
	p += TokenLen
	if !ValidToken(c.Token) {
		return JoinCode{}, fmt.Errorf("%w: token", ErrBadCode)
	}
	copy(c.Key[:], body[p:p+len(Key{})])
	p += len(Key{})
	c.Host = body[p]
	p++
	c.Port = binary.BigEndian.Uint16(body[p:])
	p += 2
	c.RPCPort = binary.BigEndian.Uint16(body[p:])
	p += 2
	n := int(body[p])
	p++
	if n > MaxEndpoints {
		return JoinCode{}, fmt.Errorf("%w: %d endpoints", ErrBadCode, n)
	}
	for range n {
		if p >= len(body) {
			return JoinCode{}, fmt.Errorf("%w: truncated endpoints", ErrBadCode)
		}
		l := int(body[p])
		p++
		if l == 0 || p+l > len(body) {
			return JoinCode{}, fmt.Errorf("%w: truncated endpoints", ErrBadCode)
		}
		e := string(body[p : p+l])
		if err := checkEndpoint(e); err != nil {
			return JoinCode{}, err
		}
		c.Endpoints = append(c.Endpoints, e)
		p += l
	}
	if p != len(body) {
		return JoinCode{}, fmt.Errorf("%w: trailing bytes", ErrBadCode)
	}
	if c.Host < FirstHost || c.Host > MaxHost {
		return JoinCode{}, fmt.Errorf("%w: host %d", ErrBadCode, c.Host)
	}
	return c, nil
}

// checkEndpoint checks an endpoint is host:port with a port; the host may be a name,
// resolved when the introduce is sent.
func checkEndpoint(e string) error {
	if _, err := netip.ParseAddrPort(e); err == nil {
		return nil
	}
	i := strings.LastIndexByte(e, ':')
	if i <= 0 || i == len(e)-1 || strings.ContainsAny(e, " /\\") {
		return fmt.Errorf("%w: endpoint %q is not host:port", ErrBadCode, e)
	}
	return nil
}
