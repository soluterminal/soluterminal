package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The private relay channel: an end-to-end encrypted, authenticated tunnel
// between an approved device and this daemon, carried by the cloud relay as
// one WebSocket (docs/DESIGN-trusted-devices-and-private-relay.md, step 2).
// The cloud sees only encrypted records and their sizes; it can neither read
// nor change what passes through, nor open a channel itself.
//
// Handshake (one JSON text message each way):
//
//	device → daemon  {v, device, target, time, nonce, eph, sig}
//	  eph: the device's ephemeral P-256 ECDH key; sig: the device's approved
//	  key over "SOLU-PRIVATE-V1-C\n target \n time \n nonce \n eph".
//	daemon → device  {eph, nonce, identity, sig}
//	  sig: this computer's identity key (daemon_identity.go), which the device
//	  pinned at enrolment, over "SOLU-PRIVATE-V1-D\n sha256(hello) \n eph \n
//	  nonce \n identity".
//
// Both sides then take ECDH(eph, eph) through HKDF-SHA256 (salt =
// sha256(hello ‖ reply)) into one AES-256-GCM key per direction. Every later
// WebSocket binary message is one record sealed under the next counter nonce.
// Records carry mux frames [stream u32][type u8][payload]; each stream is a raw
// byte pipe the device uses as an HTTP/1.1 connection, served here by the same
// handler the relay uses — WebSocket upgrades and event streams included.
//
// Frames: open (1) announces a stream before any of its data; data (2); close
// (3) is a half-close — the receiver delivers what it already holds, then
// ends the stream; ping (4); window (5, payload u32) grants the peer that many
// more bytes on the stream. Each side starts with privateStreamWindow bytes
// of credit per stream and returns credit as its app reads, so one slow
// stream never holds up the others and nothing is dropped; data beyond the
// granted credit is a protocol violation and resets that stream.

const (
	privateRelayPath       = "/api/private-relay"
	privateRelayVersion    = "SOLU-PRIVATE-V1"
	privateHelloMax        = 4 << 10
	privateFramePayloadMax = 32 << 10
	privateMaxStreams      = 256
	// privateStreamWindow is each stream's credit in each direction.
	privateStreamWindow     = 256 << 10
	privateHandshakeTimeout = 10 * time.Second
	privateIdleTimeout      = 10 * time.Minute

	privateFrameOpen   = 1
	privateFrameData   = 2
	privateFrameClose  = 3
	privateFramePing   = 4
	privateFrameWindow = 5
)

type privateHello struct {
	V      int    `json:"v"`
	Device string `json:"device"`
	Target string `json:"target"`
	Time   string `json:"time"`
	Nonce  string `json:"nonce"`
	Eph    string `json:"eph"`
	Sig    string `json:"sig"`
}

type privateReply struct {
	Eph      string `json:"eph"`
	Nonce    string `json:"nonce"`
	Identity string `json:"identity"`
	Sig      string `json:"sig"`
}

func privateHelloMessage(h privateHello) []byte {
	return []byte(strings.Join([]string{privateRelayVersion + "-C", h.Target, h.Time, h.Nonce, h.Eph}, "\n"))
}

func privateReplyMessage(helloRaw []byte, r privateReply) []byte {
	sum := sha256.Sum256(helloRaw)
	return []byte(strings.Join([]string{privateRelayVersion + "-D", hex.EncodeToString(sum[:]), r.Eph, r.Nonce, r.Identity}, "\n"))
}

// privateKeys derives the per-direction AEADs from the shared secret and the
// exact handshake bytes, so a relay that alters either message ends up with
// keys neither side holds.
func privateKeys(shared, helloRaw, replyRaw []byte) (c2s, s2c cipher.AEAD, err error) {
	salt := sha256.Sum256(append(append([]byte{}, helloRaw...), replyRaw...))
	mk := func(info string) (cipher.AEAD, error) {
		key, err := hkdf.Key(sha256.New, shared, salt[:], privateRelayVersion+" "+info, 32)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	if c2s, err = mk("c2s"); err != nil {
		return nil, nil, err
	}
	s2c, err = mk("s2c")
	return c2s, s2c, err
}

// privateRecords seals and opens records in counter order for one direction
// each. Sending is serialised so nonces and wire order always agree.
type privateRecords struct {
	ws       *websocket.Conn
	sealer   cipher.AEAD
	opener   cipher.AEAD
	sendMu   sync.Mutex
	sendSeq  uint64
	recvSeq  uint64
	aad      []byte
	deadline time.Duration
}

func privateNonce(seq uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

func (p *privateRecords) send(plain []byte) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if p.sendSeq == ^uint64(0) {
		return errors.New("private relay: record counter exhausted")
	}
	sealed := p.sealer.Seal(nil, privateNonce(p.sendSeq), plain, p.aad)
	p.sendSeq++
	_ = p.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return p.ws.WriteMessage(websocket.BinaryMessage, sealed)
}

func (p *privateRecords) recv() ([]byte, error) {
	if p.deadline > 0 {
		_ = p.ws.SetReadDeadline(time.Now().Add(p.deadline))
	}
	mt, data, err := p.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if mt != websocket.BinaryMessage {
		return nil, errors.New("private relay: unexpected message type")
	}
	plain, err := p.opener.Open(nil, privateNonce(p.recvSeq), data, p.aad)
	if err != nil {
		return nil, errors.New("private relay: record failed authentication")
	}
	p.recvSeq++
	return plain, nil
}

// privateMux carries many byte streams over one record channel. Clients open
// streams; the daemon only accepts them.
type privateMux struct {
	rec     *privateRecords
	mu      sync.Mutex
	streams map[uint32]*privateStream
	nextID  uint32
	accept  chan net.Conn // daemon side: newly opened streams
	done    chan struct{}
	once    sync.Once
}

type privateStream struct {
	id   uint32
	mu   sync.Mutex
	q    [][]byte // data from the peer not yet written to the stream
	size int
	// remoteClosed: the peer closed its side; the queue is delivered, then
	// the stream ends (half-close, so the last of a response isn't lost).
	remoteClosed bool
	// dropped: torn down; enqueue refuses further data.
	dropped bool
	// credit is how many more bytes this side may send (window frames add
	// to it); unacked is data delivered to the app not yet granted back.
	credit     int
	unacked    int
	creditWake chan struct{} // cap 1
	wake       chan struct{} // cap 1: data or remoteClosed arrived
	// gone is closed (under the mux lock, once) when the stream is torn down
	// without delivering what is queued (channel end, reset).
	gone   chan struct{}
	closed bool
}

func (st *privateStream) end() {
	if !st.closed {
		st.closed = true
		close(st.gone)
	}
}

func (st *privateStream) signal() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// next blocks until data is queued (ok), the peer closed and the queue is
// empty, or the stream was torn down (both !ok).
func (st *privateStream) next() ([]byte, bool) {
	for {
		st.mu.Lock()
		if len(st.q) > 0 {
			data := st.q[0]
			st.q[0] = nil
			st.q = st.q[1:]
			st.size -= len(data)
			st.mu.Unlock()
			return data, true
		}
		done := st.remoteClosed
		st.mu.Unlock()
		if done {
			return nil, false
		}
		select {
		case <-st.wake:
		case <-st.gone:
			return nil, false
		}
	}
}

func newPrivateMux(rec *privateRecords, server bool) *privateMux {
	m := &privateMux{rec: rec, streams: map[uint32]*privateStream{}, nextID: 1, done: make(chan struct{})}
	if server {
		m.accept = make(chan net.Conn, 64)
	}
	return m
}

func (m *privateMux) frame(id uint32, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, id)
	buf[4] = typ
	copy(buf[5:], payload)
	return m.rec.send(buf)
}

// startStream wires one stream: appSide is what the HTTP client or server
// uses; the mux pumps muxSide to and from the records.
func (m *privateMux) startStream(id uint32, announce bool) (net.Conn, error) {
	m.mu.Lock()
	if _, exists := m.streams[id]; exists || len(m.streams) >= privateMaxStreams {
		m.mu.Unlock()
		return nil, errors.New("private relay: stream refused")
	}
	st := &privateStream{id: id, wake: make(chan struct{}, 1), gone: make(chan struct{}),
		credit: privateStreamWindow, creditWake: make(chan struct{}, 1)}
	m.streams[id] = st
	m.mu.Unlock()
	if announce { // before any data can go out for it
		if err := m.frame(id, privateFrameOpen, nil); err != nil {
			m.dropStream(id)
			return nil, err
		}
	}
	appSide, muxSide := net.Pipe()
	go func() { // a torn-down stream unblocks any pending read or write
		<-st.gone
		_ = muxSide.Close()
	}()
	go func() { // records → stream
		defer muxSide.Close()
		for {
			data, ok := st.next()
			if !ok {
				st.mu.Lock()
				halfClosed := st.remoteClosed && !st.dropped
				st.mu.Unlock()
				if halfClosed { // everything delivered; the stream is over
					_ = muxSide.Close()
					m.dropStream(id)
				}
				return
			}
			if _, err := muxSide.Write(data); err != nil {
				return
			}
			st.mu.Lock()
			st.unacked += len(data)
			grant := 0
			if st.unacked >= privateStreamWindow/2 {
				grant, st.unacked = st.unacked, 0
			}
			st.mu.Unlock()
			if grant > 0 {
				var p [4]byte
				binary.BigEndian.PutUint32(p[:], uint32(grant))
				_ = m.frame(id, privateFrameWindow, p[:])
			}
		}
	}()
	go func() { // stream → records, within the peer's credit
		buf := make([]byte, privateFramePayloadMax)
		for {
			allowed, ok := st.awaitCredit()
			if !ok {
				break
			}
			if allowed > len(buf) {
				allowed = len(buf)
			}
			n, err := muxSide.Read(buf[:allowed])
			if n > 0 {
				st.mu.Lock()
				st.credit -= n
				st.mu.Unlock()
				if m.frame(id, privateFrameData, buf[:n]) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = m.frame(id, privateFrameClose, nil)
		m.dropStream(id)
	}()
	return appSide, nil
}

// awaitCredit blocks until the peer has granted credit (returning how much),
// or the stream is torn down (!ok).
func (st *privateStream) awaitCredit() (int, bool) {
	for {
		st.mu.Lock()
		c := st.credit
		st.mu.Unlock()
		if c > 0 {
			return c, true
		}
		select {
		case <-st.creditWake:
		case <-st.gone:
			return 0, false
		}
	}
}

// enqueue queues peer data for st without blocking; false when the peer sent
// more than the credit granted (or st already ended).
func (m *privateMux) enqueue(st *privateStream, data []byte) bool {
	st.mu.Lock()
	if st.dropped || st.remoteClosed || st.size+st.unacked+len(data) > privateStreamWindow {
		st.mu.Unlock()
		return false
	}
	st.q = append(st.q, data)
	st.size += len(data)
	st.mu.Unlock()
	st.signal()
	return true
}

func (m *privateMux) dropStream(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.streams[id]; ok {
		st.end()
		delete(m.streams, id)
		st.mu.Lock()
		st.q, st.size, st.dropped = nil, 0, true
		st.mu.Unlock()
	}
}

// open starts a new client stream.
func (m *privateMux) open() (net.Conn, error) {
	m.mu.Lock()
	id := m.nextID
	m.nextID += 2
	m.mu.Unlock()
	return m.startStream(id, true)
}

// run reads records until the channel ends, dispatching frames to streams.
func (m *privateMux) run() error {
	defer m.shutdown()
	for {
		rec, err := m.rec.recv()
		if err != nil {
			return err
		}
		if len(rec) < 5 {
			return errors.New("private relay: short frame")
		}
		id, typ, payload := binary.BigEndian.Uint32(rec), rec[4], rec[5:]
		switch typ {
		case privateFramePing:
			// The computer answers each ping, so a device can tell a dead
			// connection (a frozen relay) from an idle one. Devices never
			// answer pings, so this cannot loop.
			if m.accept != nil {
				if err := m.frame(0, privateFramePing, nil); err != nil {
					return err
				}
			}
		case privateFrameOpen:
			if m.accept == nil {
				return errors.New("private relay: peer opened a stream")
			}
			conn, err := m.startStream(id, false)
			if err != nil {
				_ = m.frame(id, privateFrameClose, nil)
				continue
			}
			select {
			case m.accept <- conn:
			case <-m.done:
				return nil
			}
		case privateFrameData:
			m.mu.Lock()
			st := m.streams[id]
			m.mu.Unlock()
			if st == nil {
				continue
			}
			if !m.enqueue(st, append([]byte(nil), payload...)) {
				// More than the credit we granted: reset this stream alone.
				_ = m.frame(id, privateFrameClose, nil)
				m.dropStream(id)
			}
		case privateFrameWindow:
			if len(payload) != 4 {
				return errors.New("private relay: bad window frame")
			}
			m.mu.Lock()
			st := m.streams[id]
			m.mu.Unlock()
			if st == nil {
				continue
			}
			st.mu.Lock()
			st.credit += int(binary.BigEndian.Uint32(payload))
			over := st.credit > 4*privateStreamWindow // more than was ever outstanding
			st.mu.Unlock()
			if over {
				_ = m.frame(id, privateFrameClose, nil)
				m.dropStream(id)
				continue
			}
			select {
			case st.creditWake <- struct{}{}:
			default:
			}
		case privateFrameClose:
			m.mu.Lock()
			st := m.streams[id]
			m.mu.Unlock()
			if st != nil {
				st.mu.Lock()
				st.remoteClosed = true
				st.mu.Unlock()
				st.signal()
			}
		default:
			return errors.New("private relay: unknown frame type " + strconv.Itoa(int(typ)))
		}
	}
}

func (m *privateMux) shutdown() {
	m.once.Do(func() {
		close(m.done)
		m.mu.Lock()
		for id, st := range m.streams {
			st.end()
			delete(m.streams, id)
		}
		m.mu.Unlock()
		_ = m.rec.ws.Close()
	})
}

// privateListener hands the daemon's accepted streams to an http.Server.
type privateListener struct{ m *privateMux }

type privateAddr struct{}

func (privateAddr) Network() string { return "private-relay" }
func (privateAddr) String() string  { return "private-relay" }

func (l privateListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.m.accept:
		return c, nil
	case <-l.m.done:
		return nil, net.ErrClosed
	}
}
func (l privateListener) Close() error   { l.m.shutdown(); return nil }
func (l privateListener) Addr() net.Addr { return privateAddr{} }

var privateRelayUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// The handshake authenticates the device itself; the browser origin of a
	// relayed upgrade is the cloud's and proves nothing either way.
	CheckOrigin: func(*http.Request) bool { return true },
	// A browser that offered subprotocols fails a handshake that selects
	// none. Cloud web clients offer "solu-auth" plus the "bearer.<jwt>" the
	// cloud consumes and strips; echoing the harmless name completes the
	// negotiation (as the pane socket upgrader does, ws.go).
	Subprotocols: []string{"solu-auth"},
}

// handlePrivateRelay serves GET /api/private-relay: relay transport only.
func (a *apiServer) handlePrivateRelay(w http.ResponseWriter, r *http.Request) {
	if !relayTransport(r) {
		http.Error(w, "the private relay channel is for remote devices", http.StatusForbidden)
		return
	}
	if a.privateRelayInner == nil || a.trustedDevices == nil || a.trustedDevices.daemonKey == nil {
		http.Error(w, "private relay unavailable", http.StatusServiceUnavailable)
		return
	}
	ws, err := privateRelayUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	go a.servePrivateRelay(ws)
}

func (a *apiServer) servePrivateRelay(ws *websocket.Conn) {
	ws.SetReadLimit(privateFramePayloadMax + 64)
	rec, device, err := a.privateHandshake(ws)
	if err != nil {
		// A page that navigates away or closes mid-handshake ends it with
		// 1001/1000, or 1006 when the tab drops without a close frame:
		// nothing was refused, so it isn't logged as a refusal.
		if websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
			_ = ws.Close()
			return
		}
		log.Printf("private-relay: handshake refused: %v", err)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "handshake refused"), time.Now().Add(time.Second))
		_ = ws.Close()
		return
	}
	log.Printf("private-relay: channel open for device %s", device)
	mux := newPrivateMux(rec, true)
	// Approval is rechecked on every request, and the whole channel (with any
	// WebSocket or stream inside it) ends as soon as the device is removed or
	// no longer approved.
	go func() {
		for {
			changed := a.trustedDevices.changes()
			if !a.trustedDevices.isApproved(device) {
				log.Printf("private-relay: device %s no longer approved; closing its channel", device)
				mux.shutdown()
				return
			}
			select {
			case <-changed:
			case <-mux.done:
				return
			}
		}
	}()
	inner := a.privateRelayInner
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !a.trustedDevices.isApproved(device) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":   "device_not_approved",
					"message": "This device is no longer approved on that computer. Approve it there in Settings → Privacy & security.",
				})
				return
			}
			inner.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return withPrivateRelayDevice(ctx, device)
		},
	}
	go func() { _ = srv.Serve(privateListener{m: mux}) }()
	err = mux.run()
	_ = srv.Close()
	log.Printf("private-relay: channel for device %s closed: %v", device, err)
}

type privateRelayDeviceKey struct{}

// withPrivateRelayDevice marks every request on a private channel's
// connections with the device its handshake authenticated.
func withPrivateRelayDevice(ctx context.Context, device string) context.Context {
	return context.WithValue(ctx, privateRelayDeviceKey{}, device)
}

// privateRelayDevice reports the approved device whose private channel r
// arrived on, or "".
func privateRelayDevice(r *http.Request) string {
	v, _ := r.Context().Value(privateRelayDeviceKey{}).(string)
	return v
}

func (a *apiServer) privateHandshake(ws *websocket.Conn) (*privateRecords, string, error) {
	_ = ws.SetReadDeadline(time.Now().Add(privateHandshakeTimeout))
	mt, helloRaw, err := ws.ReadMessage()
	if err != nil {
		return nil, "", err
	}
	if mt != websocket.TextMessage || len(helloRaw) > privateHelloMax {
		return nil, "", errors.New("bad hello")
	}
	var hello privateHello
	if err := json.Unmarshal(helloRaw, &hello); err != nil || hello.V != 1 {
		return nil, "", errors.New("bad hello")
	}
	s := a.trustedDevices
	unix, err := strconv.ParseInt(hello.Time, 10, 64)
	if err != nil {
		return nil, "", errDeviceStale
	}
	now := s.now()
	if d := now.Sub(time.Unix(unix, 0)); d > deviceSigMaxSkew || d < -deviceSigMaxSkew {
		return nil, "", errDeviceStale
	}
	machineID, backendID := "", ""
	if s.identity != nil {
		machineID, backendID = s.identity()
	}
	if !deviceTargetMatches(hello.Target, machineID, backendID) {
		return nil, "", errDeviceWrongTarget
	}
	ephBytes, err := base64.RawURLEncoding.DecodeString(hello.Eph)
	if err != nil {
		return nil, "", errors.New("bad ephemeral key")
	}
	clientEph, err := ecdh.P256().NewPublicKey(ephBytes)
	if err != nil {
		return nil, "", errors.New("bad ephemeral key")
	}
	s.mu.Lock()
	d := s.find(hello.Device)
	var deviceKey *ecdsa.PublicKey
	// An alerts-only device (deviceStatusApprovedAlerts) never opens it:
	// everything inside the channel counts as that device.
	if d != nil && d.Status == deviceStatusApproved {
		deviceKey = d.key
	}
	nonceKey := "private/" + hello.Device + "/" + hello.Nonce
	s.pruneNoncesLocked(now)
	_, replayed := s.nonces[nonceKey]
	if deviceKey != nil && !replayed && hello.Nonce != "" {
		s.nonces[nonceKey] = now.Add(2 * deviceSigMaxSkew)
	}
	s.mu.Unlock()
	if deviceKey == nil {
		return nil, "", errDeviceUnknown
	}
	if replayed || hello.Nonce == "" {
		return nil, "", errDeviceReplay
	}
	if !verifyDeviceSignature(deviceKey, privateHelloMessage(hello), hello.Sig) {
		return nil, "", errDeviceBadSig
	}
	identity := s.daemonKey()
	if identity == nil {
		return nil, "", errors.New("daemon identity unavailable")
	}
	daemonEph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	reply := privateReply{
		Eph:      base64.RawURLEncoding.EncodeToString(daemonEph.PublicKey().Bytes()),
		Nonce:    base64.RawURLEncoding.EncodeToString(nonce),
		Identity: identity.spki,
	}
	if reply.Sig, err = identity.sign(privateReplyMessage(helloRaw, reply)); err != nil {
		return nil, "", err
	}
	replyRaw, err := json.Marshal(reply)
	if err != nil {
		return nil, "", err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(privateHandshakeTimeout))
	if err := ws.WriteMessage(websocket.TextMessage, replyRaw); err != nil {
		return nil, "", err
	}
	shared, err := daemonEph.ECDH(clientEph)
	if err != nil {
		return nil, "", err
	}
	c2s, s2c, err := privateKeys(shared, helloRaw, replyRaw)
	if err != nil {
		return nil, "", err
	}
	_ = ws.SetReadDeadline(time.Time{})
	return &privateRecords{ws: ws, sealer: s2c, opener: c2s, aad: []byte(privateRelayVersion), deadline: privateIdleTimeout}, hello.Device, nil
}

// privateClientHandshake is the device side, used by the Go tests and by any
// Go client (the web and mobile clients implement the same exchange).
func privateClientHandshake(ws *websocket.Conn, deviceID, target string, sign func([]byte) string, pinnedIdentity string, now time.Time) (*privateRecords, error) {
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello := privateHello{V: 1, Device: deviceID, Target: target, Time: strconv.FormatInt(now.Unix(), 10),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Eph: base64.RawURLEncoding.EncodeToString(eph.PublicKey().Bytes())}
	hello.Sig = sign(privateHelloMessage(hello))
	helloRaw, err := json.Marshal(hello)
	if err != nil {
		return nil, err
	}
	if err := ws.WriteMessage(websocket.TextMessage, helloRaw); err != nil {
		return nil, err
	}
	mt, replyRaw, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if mt != websocket.TextMessage {
		return nil, errors.New("private relay: bad reply")
	}
	var reply privateReply
	if err := json.Unmarshal(replyRaw, &reply); err != nil {
		return nil, err
	}
	if reply.Identity != pinnedIdentity {
		return nil, errors.New("private relay: the computer's identity is not the one approved")
	}
	idKey, err := parseDevicePublicKey(reply.Identity)
	if err != nil {
		return nil, err
	}
	sigMsg := privateReplyMessage(helloRaw, privateReply{Eph: reply.Eph, Nonce: reply.Nonce, Identity: reply.Identity})
	if !verifyDeviceSignature(idKey, sigMsg, reply.Sig) {
		return nil, errors.New("private relay: the computer's signature does not match")
	}
	peerBytes, err := base64.RawURLEncoding.DecodeString(reply.Eph)
	if err != nil {
		return nil, err
	}
	peer, err := ecdh.P256().NewPublicKey(peerBytes)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(peer)
	if err != nil {
		return nil, err
	}
	c2s, s2c, err := privateKeys(shared, helloRaw, replyRaw)
	if err != nil {
		return nil, err
	}
	return &privateRecords{ws: ws, sealer: c2s, opener: s2c, aad: []byte(privateRelayVersion)}, nil
}
