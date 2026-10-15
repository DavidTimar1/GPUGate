// Package sshtunnel holds one SSH connection per Verda machine and a loopback listener whose
// every accepted connection becomes a direct-tcpip channel to the machine's own 127.0.0.1:8000
// (only port 22 answers on a Verda machine, docs/providers.md#verda). No other package imports
// golang.org/x/crypto/ssh.
//
// The private key is named by path only and its content is never written to a log. The
// machine's host key is pinned on first connect and handed back through onHostKey so the caller
// can store it; every later reconnect in this process, and every reconnect after a restart that
// supplies the same pin, is checked against it. Verda publishes no host key to check a first
// connect against. [DOC] https://api.verda.com/v1/openapi.json, read 2026-09-21: zero hits for
// host_key, hostkey or known_hosts.
package sshtunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// PrivateKeyPath is the one SSH private key this program signs with. Like the other working-state
// paths (internal/podkey's dir, cmd/gpugate/main.go's dbPath) it is a constant, not a value read
// from .env. The key is untracked and must have no passphrase.
const PrivateKeyPath = ".ssh/verda/private"

const (
	dialTimeout         = 10 * time.Second
	handshakeTimeout    = 20 * time.Second
	reconnectInterval   = 15 * time.Second
	keepaliveInterval   = 30 * time.Second
	keepaliveTimeout    = 15 * time.Second
	oneHourCeiling      = time.Hour
	remoteForwardTarget = "127.0.0.1:8000"
)

// Key is the private key every Tunnel signs with, loaded once and held for the life of the
// process. Its own ssh.Signer is unexported so nothing outside this package ever handles an
// x/crypto/ssh type.
type Key struct {
	signer ssh.Signer
}

// LoadKey reads PrivateKeyPath and parses it as an SSH private key. The key must have no
// passphrase, because nothing here can ask for one, and its file should be mode 0600. Its own
// content is never read into an error: only the path.
func LoadKey() (*Key, error) {
	raw, err := os.ReadFile(PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read the SSH key at %s: %w", PrivateKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("the SSH key at %s could not be parsed: %w", PrivateKeyPath, err)
	}
	return &Key{signer: signer}, nil
}

// IsRegistered reports whether this key's public half equals one of authorizedLines, each one
// line of the "key" field GET /v1/sshkeys answers, in OpenSSH authorized_keys form. A line that
// does not parse is skipped, never treated as a match or an error: the account may hold a key
// type this package does not need to understand.
func (k *Key) IsRegistered(authorizedLines []string) bool {
	if k == nil {
		return false
	}
	mine := k.signer.PublicKey().Marshal()
	for _, line := range authorizedLines {
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if bytes.Equal(parsed.Marshal(), mine) {
			return true
		}
	}
	return false
}

// Tunnel is one SSH connection from this program to one Verda machine's port 22, and a loopback
// listener forwarding every accepted connection to that machine's own 127.0.0.1:8000. Its local
// port is fixed at New and stays stable for the life of the Tunnel, whatever the SSH connection
// does (a dropped tunnel is rebuilt on the same local port).
type Tunnel struct {
	sshAddr   string
	key       *Key
	onHostKey func(authorizedLine string)
	logf      func(text string)

	listener  net.Listener
	localPort int

	mu            sync.Mutex
	client        *ssh.Client
	connected     bool
	lastAttempt   string
	pinnedHostKey string // "" until the first connect pins one, in-process or from the caller
	// closed is set under mu by Close, together with reading the client Close closes. It is the
	// one flag setConnected checks, under the same lock, before storing a client that a connect
	// attempt in flight hands it. This makes Close win a race against a connect attempt that was
	// already dialing when Close ran: whichever of the two takes mu first decides the outcome, so
	// a client that finishes connecting after Close ran is closed at once instead of being stored
	// and logged as up.
	closed bool

	closeCh   chan struct{}
	closeOnce sync.Once
}

// New starts one Tunnel to sshAddr ("<ip>:22"), signing with key. pinnedHostKey is the machine's
// host key, in OpenSSH authorized_keys form, already on file for it ("" for a machine never
// connected to before); onHostKey is called once, with the newly pinned line, the first time this
// Tunnel accepts a host key with none pinned yet. logf receives one line per state change, never
// the machine's address or any key value. The local listener is bound before this returns, so
// LocalBaseURL is usable at once even though the machine may not answer SSH yet.
func New(sshAddr string, key *Key, pinnedHostKey string, onHostKey func(authorizedLine string), logf func(text string)) (*Tunnel, error) {
	if key == nil {
		return nil, fmt.Errorf("no SSH key to open a tunnel with")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open a local listener for the tunnel: %w", err)
	}
	t := &Tunnel{
		sshAddr:       sshAddr,
		key:           key,
		onHostKey:     onHostKey,
		logf:          logf,
		listener:      listener,
		localPort:     listener.Addr().(*net.TCPAddr).Port,
		lastAttempt:   "not yet tried",
		pinnedHostKey: pinnedHostKey,
		closeCh:       make(chan struct{}),
	}
	go t.acceptLoop()
	go t.connectLoop()
	return t, nil
}

// LocalBaseURL is "http://127.0.0.1:<port>", fixed at New: every reader builds its outgoing
// request from this URL as it would from a provider's ServingBaseURL.
func (t *Tunnel) LocalBaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(t.localPort)
}

// Addr answers the machine's "<ip>:22" this Tunnel was built to reach; callers never log it.
// The console's ensureTunnel reads it to notice when a fresh answer names a different ip and the
// tunnel must be rebuilt.
func (t *Tunnel) Addr() string {
	return t.sshAddr
}

// Connected reports whether the SSH connection is up right now.
func (t *Tunnel) Connected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected
}

// LastAttempt answers the classification of the most recent connect attempt or drop: "connection
// refused", "timed out", "key refused", "host key mismatch", "could not connect", or "" while
// connected. Never the machine's address or any error text that could carry it.
func (t *Tunnel) LastAttempt() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAttempt
}

// Exec runs cmd on the machine over the current SSH connection, one session, combined output. It
// answers an error at once when the tunnel is not connected, rather than waiting on a connection
// that may not come back for up to an hour.
func (t *Tunnel) Exec(ctx context.Context, cmd string) ([]byte, error) {
	t.mu.Lock()
	client := t.client
	t.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("the SSH tunnel to the box is not connected")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open an SSH session: %w", err)
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := session.CombinedOutput(cmd)
		done <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return nil, ctx.Err()
	case r := <-done:
		return r.out, r.err
	}
}

// Close stops the connect and accept loops, and closes the listener and the SSH client. Safe to
// call more than once; logs "SSH tunnel closed" exactly once, whichever caller's Close actually
// runs the close. Every field a race with a still-connecting attempt could touch (closed, client)
// is read and written together, under mu, so setConnected below can never store a client that
// lost a race against this call.
func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.closeCh)
		_ = t.listener.Close()
		t.mu.Lock()
		t.closed = true
		client := t.client
		t.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
		t.logf("SSH tunnel closed")
	})
}

// connectLoop dials sshAddr, reconnecting on every drop, for at most oneHourCeiling of continuous
// failure counted from the last drop (or from New, before any connection has ever succeeded).
// Never sleeps between a drop and the next attempt, only between two failed attempts.
func (t *Tunnel) connectLoop() {
	deadline := time.Now().Add(oneHourCeiling)
	for {
		select {
		case <-t.closeCh:
			return
		default:
		}

		client, err := t.attemptConnect()
		if err != nil {
			class := classifyDialErr(err)
			t.setLastAttempt(class)
			if class == "host key mismatch" {
				t.markDead("the box's host key does not match the one pinned for it")
				return
			}
			if time.Now().After(deadline) {
				t.markDead("could not connect within the one-hour ceiling")
				return
			}
			select {
			case <-t.closeCh:
				return
			case <-time.After(reconnectInterval):
			}
			continue
		}

		if !t.setConnected(client) {
			// Close ran while this attempt was already dialing: the client above is already
			// closed, and this loop stops here without logging "up" and without attempting
			// another connect (the accept loop's listener is already closed too, so there is
			// nothing left for a connection to forward to).
			return
		}
		t.logf("SSH tunnel up on local port " + strconv.Itoa(t.localPort))
		stopKeepalive := t.startKeepalive(client)
		waitErr := client.Wait()
		_ = waitErr
		stopKeepalive()
		t.clearConnected()

		select {
		case <-t.closeCh:
			return
		default:
		}
		t.logf("SSH tunnel dropped, reconnecting")
		deadline = time.Now().Add(oneHourCeiling)
	}
}

// attemptConnect dials sshAddr and completes the SSH handshake: a 10-second bound on the TCP
// dial [DOC] x/crypto v0.57.0 ssh/client.go:203-214, ssh.Dial only bounds the TCP dial, never the
// handshake, so a 20-second deadline is set around ssh.NewClientConn by hand and cleared once it
// returns.
func (t *Tunnel) attemptConnect() (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", t.sshAddr, dialTimeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(t.key.signer)},
		HostKeyCallback: t.hostKeyCallback(),
		Timeout:         dialTimeout,
	}
	sconn, chans, reqs, err := ssh.NewClientConn(conn, t.sshAddr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sconn.Close()
		return nil, err
	}
	return ssh.NewClient(sconn, chans, reqs), nil
}

// hostKeyCallback checks the machine's host key against the pin: ssh.FixedHostKey when one is
// already held, or accept-and-remember on the first connect this Tunnel makes. The fingerprint
// logged at pin time is public, so a by-hand ssh-keyscan can be compared against it.
func (t *Tunnel) hostKeyCallback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		t.mu.Lock()
		pin := t.pinnedHostKey
		t.mu.Unlock()
		if pin != "" {
			parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pin))
			if err != nil {
				return fmt.Errorf("ssh: the pinned host key on file could not be parsed")
			}
			return ssh.FixedHostKey(parsed)(hostname, remote, key)
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		t.mu.Lock()
		t.pinnedHostKey = line
		t.mu.Unlock()
		t.logf("pinned the box's SSH host key, fingerprint " + ssh.FingerprintSHA256(key))
		if t.onHostKey != nil {
			t.onHostKey(line)
		}
		return nil
	}
}

// startKeepalive sends one keepalive@openssh.com request every keepaliveInterval, closing client
// when a reply takes longer than keepaliveTimeout or the request itself errors [DOC] x/crypto
// v0.57.0 ssh/connection.go:56. Returns a function that stops the goroutine; the caller stops it
// once client.Wait() has returned, whatever ended the connection.
func (t *Tunnel) startKeepalive(client *ssh.Client) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				done := make(chan error, 1)
				go func() {
					_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
					done <- err
				}()
				select {
				case err := <-done:
					if err != nil {
						_ = client.Close()
						return
					}
				case <-time.After(keepaliveTimeout):
					_ = client.Close()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}

// acceptLoop accepts local connections on the listener bound in New and forwards each one, until
// the listener is closed (Close).
func (t *Tunnel) acceptLoop() {
	for {
		lc, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.forward(lc)
	}
}

// forward carries one local connection to the box's own loopback, over the current SSH
// connection's direct-tcpip channel [DOC] x/crypto v0.57.0 ssh/tcpip.go:380. A tunnel with no
// current connection closes lc at once, so the HTTP client on the other end gets an immediate
// error rather than a hang.
func (t *Tunnel) forward(lc net.Conn) {
	t.mu.Lock()
	client := t.client
	t.mu.Unlock()
	if client == nil {
		_ = lc.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	rc, err := client.DialContext(ctx, "tcp", remoteForwardTarget)
	if err != nil {
		_ = lc.Close()
		return
	}
	go copyAndClose(rc, lc)
	go copyAndClose(lc, rc)
}

// copyAndClose copies src into dst and closes both ends once its direction ends. Two of
// these, one per direction, are how forward carries a connection both ways.
func copyAndClose(dst io.WriteCloser, src io.ReadCloser) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// setConnected stores client as the tunnel's current connection and reports true, unless Close
// has already run, in which case the client that just connected has already lost: it is closed
// at once, never stored, and this reports false so the caller stops instead of logging "up" and
// looping again.
func (t *Tunnel) setConnected(client *ssh.Client) bool {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = client.Close()
		return false
	}
	t.client = client
	t.connected = true
	t.lastAttempt = ""
	t.mu.Unlock()
	return true
}

func (t *Tunnel) clearConnected() {
	t.mu.Lock()
	t.client = nil
	t.connected = false
	t.mu.Unlock()
}

func (t *Tunnel) setLastAttempt(s string) {
	t.mu.Lock()
	t.lastAttempt = s
	t.mu.Unlock()
}

func (t *Tunnel) markDead(reason string) {
	t.mu.Lock()
	t.lastAttempt = "could not connect"
	t.mu.Unlock()
	t.logf("SSH tunnel to the box is dead: " + reason)
}

// classifyDialErr turns a dial or handshake error into one of the words logReadinessStatus and
// the strip read, never the error's own text: an SSH dial error embeds the address it dialed,
// which must never reach the log.
func classifyDialErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	text := err.Error()
	if strings.Contains(text, "unable to authenticate") {
		return "key refused"
	}
	if strings.Contains(text, "host key mismatch") {
		return "host key mismatch"
	}
	return "could not connect"
}
