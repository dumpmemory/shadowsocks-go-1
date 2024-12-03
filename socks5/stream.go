package socks5

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"

	"github.com/database64128/shadowsocks-go/conn"
	"github.com/database64128/shadowsocks-go/zerocopy"
)

// SOCKS version 5.
const Version = 5

// UnsupportedVersionError is an error type for unsupported SOCKS versions.
type UnsupportedVersionError byte

func (v UnsupportedVersionError) Error() string {
	return fmt.Sprintf("unsupported SOCKS version: %#X", v)
}

func (UnsupportedVersionError) Is(target error) bool {
	return target == errors.ErrUnsupported
}

// SOCKS5 authentication methods as defined in RFC 1928 section 3.
const (
	MethodNoAuthenticationRequired = 0
	MethodGSSAPI                   = 1
	MethodUsernamePassword         = 2
	MethodNoAcceptable             = 0xFF
)

// UnsupportedAuthMethodError is an error type for unsupported SOCKS5 authentication methods.
type UnsupportedAuthMethodError byte

func (m UnsupportedAuthMethodError) Error() string {
	return fmt.Sprintf("unsupported authentication method: %#X", m)
}

func (UnsupportedAuthMethodError) Is(target error) bool {
	return target == errors.ErrUnsupported
}

// SOCKS5 request commands as defined in RFC 1928 section 4.
const (
	CmdConnect      = 1
	CmdBind         = 2
	CmdUDPAssociate = 3
)

// UnsupportedCommandError is an error type for unsupported SOCKS5 request commands.
type UnsupportedCommandError byte

func (c UnsupportedCommandError) Error() string {
	return fmt.Sprintf("unsupported command: %#X", c)
}

func (UnsupportedCommandError) Is(target error) bool {
	return target == errors.ErrUnsupported
}

// SOCKS5 reply field values as defined in RFC 1928 section 6.
const (
	ReplySucceeded                     = 0
	ReplyGeneralSocksServerFailure     = 1
	ReplyConnectionNotAllowedByRuleset = 2
	ReplyNetworkUnreachable            = 3
	ReplyHostUnreachable               = 4
	ReplyConnectionRefused             = 5
	ReplyTTLExpired                    = 6
	ReplyCommandNotSupported           = 7
	ReplyAddressTypeNotSupported       = 8
)

// ReplyError is an error type for SOCKS5 reply errors.
type ReplyError byte

func (r ReplyError) Error() string {
	switch r {
	case ReplySucceeded:
		return "succeeded"
	case ReplyGeneralSocksServerFailure:
		return "general SOCKS server failure"
	case ReplyConnectionNotAllowedByRuleset:
		return "connection not allowed by ruleset"
	case ReplyNetworkUnreachable:
		return "network unreachable"
	case ReplyHostUnreachable:
		return "host unreachable"
	case ReplyConnectionRefused:
		return "connection refused"
	case ReplyTTLExpired:
		return "TTL expired"
	case ReplyCommandNotSupported:
		return "command not supported"
	case ReplyAddressTypeNotSupported:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown SOCKS5 reply error: %#X", r)
	}
}

// UsernamePasswordAuthVersion is the version of the username/password authentication method,
// as defined in RFC 1929 section 2.
const UsernamePasswordAuthVersion = 1

// UnsupportedUsernamePasswordAuthVersionError is an error type for unsupported username/password authentication versions.
type UnsupportedUsernamePasswordAuthVersionError byte

func (v UnsupportedUsernamePasswordAuthVersionError) Error() string {
	return fmt.Sprintf("unsupported username/password authentication version: %#X", v)
}

func (UnsupportedUsernamePasswordAuthVersionError) Is(target error) bool {
	return target == errors.ErrUnsupported
}

var (
	ErrUsernameLengthOutOfRange  = errors.New("username length out of range [1, 255]")
	ErrPasswordLengthOutOfRange  = errors.New("password length out of range [1, 255]")
	ErrNoAcceptableAuthMethod    = errors.New("no acceptable authentication method")
	ErrIncorrectUsernamePassword = errors.New("incorrect username or password")
	ErrUDPAssociateDone          = errors.New("UDP ASSOCIATE done")
	errZeroNMETHODS              = errors.New("NMETHODS is 0")
	errZeroULEN                  = errors.New("ULEN is 0")
	errZeroPLEN                  = errors.New("PLEN is 0")
)

// UserInfo is a username/password pair.
type UserInfo struct {
	// Username is the username.
	// It must be non-empty and at most 255 bytes long.
	Username string `json:"username"`

	// Password is the password.
	// It must be non-empty and at most 255 bytes long.
	Password string `json:"password"`
}

// Validate checks if the username and password are valid.
func (u UserInfo) Validate() error {
	if len(u.Username) == 0 || len(u.Username) > 255 {
		return ErrUsernameLengthOutOfRange
	}
	if len(u.Password) == 0 || len(u.Password) > 255 {
		return ErrPasswordLengthOutOfRange
	}
	return nil
}

// AuthMsgLength returns the length of the authentication message for the username/password pair.
func (u UserInfo) AuthMsgLength() int {
	return 1 + 1 + len(u.Username) + 1 + len(u.Password)
}

// AppendAuthMsg appends the username/password pair as an authentication message to b.
//
// Call Validate first to ensure the username and password are valid.
func (u UserInfo) AppendAuthMsg(b []byte) []byte {
	b = slices.Grow(b, u.AuthMsgLength())
	b = append(b, UsernamePasswordAuthVersion, byte(len(u.Username)))
	b = append(b, u.Username...)
	b = append(b, byte(len(u.Password)))
	b = append(b, u.Password...)
	return b
}

// replyWithStatus writes a reply to w with the REP field set to status.
func replyWithStatus(w io.Writer, b []byte, status byte) error {
	const replyLen = 3 + IPv4AddrLen
	reply := b[:replyLen]
	reply[0] = Version
	reply[1] = status
	reply[2] = 0
	*(*[IPv4AddrLen]byte)(reply[3:]) = IPv4UnspecifiedAddr
	_, err := w.Write(reply)
	return err
}

// clientNegotiateAuthMethod negotiates the authentication method with the server.
//
// len(b) must be at least 3.
func clientNegotiateAuthMethod(rw io.ReadWriter, b []byte, method byte) error {
	if len(b) < 3 {
		panic("clientNegotiateAuthMethod: buffer too small")
	}

	// Put and write VER, NMETHODS, METHOD.
	b[0] = Version
	b[1] = 1
	b[2] = method
	if _, err := rw.Write(b[:3]); err != nil {
		return err
	}

	// Read VER, METHOD.
	if _, err := io.ReadFull(rw, b[:2]); err != nil {
		return err
	}

	// Check VER.
	if b[0] != Version {
		return UnsupportedVersionError(b[0])
	}

	// Check METHOD.
	if b[1] != method {
		return UnsupportedAuthMethodError(b[1])
	}

	return nil
}

// clientDoUsernamePasswordAuth performs the username/password authentication.
//
// len(b) must be at least 2.
func clientDoUsernamePasswordAuth(rw io.ReadWriter, b, authMsg []byte) error {
	if len(b) < 2 {
		panic("clientDoUsernamePasswordAuth: buffer too small")
	}

	// Write authMsg.
	if _, err := rw.Write(authMsg); err != nil {
		return err
	}

	// Read VER, STATUS.
	if _, err := io.ReadFull(rw, b[:2]); err != nil {
		return err
	}

	// Check VER.
	if b[0] != UsernamePasswordAuthVersion {
		return UnsupportedUsernamePasswordAuthVersionError(b[0])
	}

	// Check STATUS.
	if b[1] != 0 {
		return ErrIncorrectUsernamePassword
	}

	return nil
}

// clientDoRequest writes a request with the given command and target address to rw.
// It returns the bound address in the reply.
//
// len(b) must be at least 3+MaxAddrLen.
func clientDoRequest(rw io.ReadWriter, b []byte, command byte, targetAddr conn.Addr) (addr conn.Addr, err error) {
	if len(b) < 3+MaxAddrLen {
		panic("clientDoRequest: buffer too small")
	}

	// Put and write VER, CMD, RSV, SOCKS address.
	b[0] = Version
	b[1] = command
	b[2] = 0
	n := WriteAddrFromConnAddr(b[3:], targetAddr)
	if _, err = rw.Write(b[:3+n]); err != nil {
		return conn.Addr{}, err
	}

	// Read VER, REP, RSV, ATYP, and an extra byte.
	if _, err = io.ReadFull(rw, b[:5]); err != nil {
		return conn.Addr{}, err
	}

	// Check VER.
	if b[0] != Version {
		return conn.Addr{}, UnsupportedVersionError(b[0])
	}

	// Check REP.
	if b[1] != ReplySucceeded {
		return conn.Addr{}, ReplyError(b[1])
	}

	// Read SOCKS address.
	sa, err := AppendFromReader(b[3:3], newPrefixedReader(b[3:5], rw))
	if err != nil {
		return conn.Addr{}, err
	}
	addr, _, err = ConnAddrFromSlice(sa)
	return addr, err
}

// ClientRequest completes the handshake and writes a request with the given command and target address to rw.
// It returns the bound address in the reply.
func ClientRequest(rw io.ReadWriter, command byte, targetAddr conn.Addr) (addr conn.Addr, err error) {
	b := make([]byte, 3+MaxAddrLen)
	if err = clientNegotiateAuthMethod(rw, b, MethodNoAuthenticationRequired); err != nil {
		return conn.Addr{}, err
	}
	return clientDoRequest(rw, b, command, targetAddr)
}

// ClientRequestUsernamePassword is like [ClientRequest], but uses username/password authentication.
func ClientRequestUsernamePassword(rw io.ReadWriter, authMsg []byte, command byte, targetAddr conn.Addr) (addr conn.Addr, err error) {
	b := make([]byte, 3+MaxAddrLen) // enough for clientNegotiateAuthMethod
	if err = clientNegotiateAuthMethod(rw, b, MethodUsernamePassword); err != nil {
		return conn.Addr{}, err
	}
	if err = clientDoUsernamePasswordAuth(rw, b, authMsg); err != nil {
		return conn.Addr{}, err
	}
	return clientDoRequest(rw, b, command, targetAddr)
}

// ClientConnect writes a CONNECT request to targetAddr.
func ClientConnect(rw io.ReadWriter, targetAddr conn.Addr) error {
	_, err := ClientRequest(rw, CmdConnect, targetAddr)
	return err
}

// ClientConnectUsernamePassword is like [ClientConnect], but uses username/password authentication.
func ClientConnectUsernamePassword(rw io.ReadWriter, authMsg []byte, targetAddr conn.Addr) error {
	_, err := ClientRequestUsernamePassword(rw, authMsg, CmdConnect, targetAddr)
	return err
}

// ClientUDPAssociate writes a UDP ASSOCIATE request to targetAddr.
func ClientUDPAssociate(rw io.ReadWriter, targetAddr conn.Addr) (conn.Addr, error) {
	return ClientRequest(rw, CmdUDPAssociate, targetAddr)
}

// ClientUDPAssociateUsernamePassword is like [ClientUDPAssociate], but uses username/password authentication.
func ClientUDPAssociateUsernamePassword(rw io.ReadWriter, authMsg []byte, targetAddr conn.Addr) (conn.Addr, error) {
	return ClientRequestUsernamePassword(rw, authMsg, CmdUDPAssociate, targetAddr)
}

// ServerAccept processes an incoming request from rw.
//
// enableTCP enables the CONNECT command.
// enableUDP enables the UDP ASSOCIATE command.
//
// When UDP is enabled, rw must be a [*net.TCPConn].
func ServerAccept(rw io.ReadWriter, enableTCP, enableUDP bool) (addr conn.Addr, err error) {
	b := make([]byte, 3+MaxAddrLen)
	if err = serverHandleMethodSelection(rw, b, MethodNoAuthenticationRequired); err != nil {
		return conn.Addr{}, err
	}
	return serverHandleRequest(rw, b, enableTCP, enableUDP)
}

// ServerAcceptUsernamePassword is like [ServerAccept], but uses username/password authentication.
func ServerAcceptUsernamePassword(rw io.ReadWriter, userInfoByUsername map[string]UserInfo, enableTCP, enableUDP bool) (addr conn.Addr, username string, err error) {
	b := make([]byte, 3+MaxAddrLen) // enough for serverHandleUsernamePassword
	if err = serverHandleMethodSelection(rw, b, MethodUsernamePassword); err != nil {
		return conn.Addr{}, "", err
	}
	username, err = serverHandleUsernamePassword(rw, b, userInfoByUsername)
	if err != nil {
		return conn.Addr{}, username, err
	}
	addr, err = serverHandleRequest(rw, b, enableTCP, enableUDP)
	return addr, username, err
}

// serverHandleMethodSelection processes an incoming version identifier and method selection message from rw.
//
// len(b) must be at least 1+1+255.
func serverHandleMethodSelection(rw io.ReadWriter, b []byte, method byte) error {
	if len(b) < 1+1+255 {
		panic("serverHandleMethodSelection: buffer too small")
	}

	// Read VER, NMETHODS, and the first METHOD.
	//
	//	+----+----------+----------+
	//	|VER | NMETHODS | METHODS  |
	//	+----+----------+----------+
	//	| 1  |    1     | 1 to 255 |
	//	+----+----------+----------+
	if _, err := io.ReadFull(rw, b[:3]); err != nil {
		return err
	}

	// Check VER.
	if b[0] != Version {
		return UnsupportedVersionError(b[0])
	}

	// Check NMETHODS and read the remaining METHODS.
	methodIndex := 0
	switch nmethods := int(b[1]); nmethods {
	case 0:
		return errZeroNMETHODS
	case 1:
		if b[2] != method {
			methodIndex = -1
		}
	default:
		if _, err := io.ReadFull(rw, b[3:3+nmethods-1]); err != nil {
			return err
		}
		methodIndex = bytes.IndexByte(b[2:2+nmethods], method)
	}
	if methodIndex == -1 {
		// b[0] is already Version.
		b[1] = MethodNoAcceptable
		_, _ = rw.Write(b[:2])
		return ErrNoAcceptableAuthMethod
	}

	// Write method selection message.
	//
	// 	+-----+--------+
	// 	| VER | METHOD |
	// 	+-----+--------+
	// 	|  1  |   1    |
	// 	+-----+--------+
	//
	// b[0] is already Version.
	b[1] = method
	_, err := rw.Write(b[:2])
	return err
}

// serverHandleUsernamePassword processes an incoming username/password authentication message from rw.
// It returns the username if the authentication is successful.
//
// len(b) must be at least 1+1+255+1.
func serverHandleUsernamePassword(rw io.ReadWriter, b []byte, userInfoByUsername map[string]UserInfo) (string, error) {
	if len(b) < 1+1+255+1 {
		panic("serverHandleUsernamePassword: buffer too small")
	}

	// Read VER, ULEN, and 2 more bytes.
	//
	//	+----+------+----------+------+----------+
	//	|VER | ULEN |  UNAME   | PLEN |  PASSWD  |
	//	+----+------+----------+------+----------+
	//	| 1  |  1   | 1 to 255 |  1   | 1 to 255 |
	//	+----+------+----------+------+----------+
	if _, err := io.ReadFull(rw, b[:4]); err != nil {
		return "", err
	}

	// Check VER.
	if b[0] != UsernamePasswordAuthVersion {
		return "", UnsupportedUsernamePasswordAuthVersionError(b[0])
	}

	// Check ULEN.
	ulen := int(b[1])
	if ulen == 0 {
		return "", errZeroULEN
	}

	// Read the remaining UNAME, PLEN.
	if ulen > 1 {
		if _, err := io.ReadFull(rw, b[4:4+ulen-1]); err != nil {
			return "", err
		}
	}

	plenIndex := 2 + ulen

	// Check UNAME.
	uname := b[2:plenIndex]
	userInfo, ok := userInfoByUsername[string(uname)]

	// Check PLEN.
	plen := int(b[plenIndex])
	if plen == 0 {
		return "", errZeroPLEN
	}

	// Read PASSWD, overwriting UNAME.
	passwd := b[2 : 2+plen]
	if _, err := io.ReadFull(rw, passwd); err != nil {
		return "", err
	}

	// status is the response status.
	// A non-zero status indicates a failure.
	var status byte

	// Check PASSWD and put STATUS.
	if !ok || string(passwd) != userInfo.Password {
		status = 1
	}

	// Write response.
	//
	//	+----+--------+
	//	|VER | STATUS |
	//	+----+--------+
	//	| 1  |   1    |
	//	+----+--------+
	//
	// b[0] is already Version.
	b[1] = status
	_, err := rw.Write(b[:2])
	if status != 0 {
		return "", ErrIncorrectUsernamePassword
	}
	return userInfo.Username, err
}

// serverHandleRequest processes an incoming request from rw, after the authentication negotiation is done.
//
// len(b) must be at least 3+[MaxAddrLen].
//
// A request looks like:
//
//	+----+-----+-------+------+----------+----------+
//	|VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
//	+----+-----+-------+------+----------+----------+
//	| 1  |  1  | X'00' |  1   | Variable |    2     |
//	+----+-----+-------+------+----------+----------+
//
// The reply looks like:
//
//	+----+-----+-------+------+----------+----------+
//	|VER | REP |  RSV  | ATYP | BND.ADDR | BND.PORT |
//	+----+-----+-------+------+----------+----------+
//	| 1  |  1  | X'00' |  1   | Variable |    2     |
//	+----+-----+-------+------+----------+----------+
func serverHandleRequest(rw io.ReadWriter, b []byte, enableTCP, enableUDP bool) (addr conn.Addr, err error) {
	if len(b) < 3+MaxAddrLen {
		panic("serverHandleRequest: buffer too small")
	}

	// Read VER, CMD, RSV, ATYP, and an extra byte.
	if _, err = io.ReadFull(rw, b[:5]); err != nil {
		return conn.Addr{}, err
	}

	// Check VER.
	if b[0] != Version {
		return conn.Addr{}, UnsupportedVersionError(b[0])
	}

	// Read SOCKS address.
	sa, err := AppendFromReader(b[3:3], newPrefixedReader(b[3:5], rw))
	if err != nil {
		return conn.Addr{}, err
	}
	addr, _, err = ConnAddrFromSlice(sa)
	if err != nil {
		return conn.Addr{}, err
	}

	cmd := b[1]
	switch {
	case cmd == CmdConnect && enableTCP:
		return addr, replyWithStatus(rw, b, ReplySucceeded)

	case cmd == CmdUDPAssociate && enableUDP:
		// Use the connection's local address as the returned UDP bound address.
		tc, ok := rw.(*net.TCPConn)
		if !ok {
			return addr, zerocopy.ErrAcceptRequiresTCPConn
		}
		localAddrPort := tc.LocalAddr().(*net.TCPAddr).AddrPort()

		// Construct reply.
		b[1] = ReplySucceeded
		reply := AppendAddrFromAddrPort(b[:3], localAddrPort)

		// Write reply.
		if _, err = rw.Write(reply); err != nil {
			return addr, err
		}

		// Hold the connection open.
		if _, err = rw.Read(b[:1]); err != nil && err != io.EOF {
			return addr, err
		}
		return addr, ErrUDPAssociateDone

	default:
		_ = replyWithStatus(rw, b, ReplyCommandNotSupported)
		return addr, UnsupportedCommandError(cmd)
	}
}

// prefixedReader is an [io.Reader] that reads from a prefix buffer first, then from an underlying reader.
type prefixedReader struct {
	prefix []byte
	r      io.Reader
}

// newPrefixedReader returns a new prefixed reader.
func newPrefixedReader(prefix []byte, r io.Reader) *prefixedReader {
	return &prefixedReader{prefix: prefix, r: r}
}

// Read implements [io.Reader.Read].
func (r *prefixedReader) Read(p []byte) (n int, err error) {
	if len(r.prefix) > 0 {
		n = copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.r.Read(p)
}
