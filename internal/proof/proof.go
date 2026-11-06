// Package proof holds every use of Noise: the pairing proof, the sessions after it, and the boxes
// two consoles seal for each other. PROTOCOL.md is the definition, and each function here says
// which part of it it is. Nothing else in the server touches a key.
package proof

import (
	"crypto/rand"
	"encoding/binary"
	"errors"

	"github.com/flynn/noise"
	"golang.org/x/crypto/chacha20poly1305"
)

// The version numbers stated on connect. Only a real break moves one.
const (
	Protocol = 1
	Oldest   = 1
	Stream   = 1
)

// KeySize is the length of a public key.
const KeySize = 32

// SessionIDSize is the length of a session's id on the wire.
const SessionIDSize = 8

// PairIDSize is the length of a pairing's id on the wire.
const PairIDSize = 16

var (
	suite           = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)
	pairPrologue    = []byte("cabinet-linkup pair 1")
	sessionPrologue = []byte("cabinet-linkup session 1")
	boxPrologue     = "cabinet-linkup box 1 "

	// ErrProof means someone did not hold what they had to: a code, or a key.
	ErrProof = errors.New("the proof failed")
)

// KeyPair is one side's long-term X25519 key.
type KeyPair struct {
	Private []byte
	Public  []byte
}

func (k KeyPair) dh() noise.DHKey {
	return noise.DHKey{Private: k.Private, Public: k.Public}
}

// NewKeyPair makes a long-term key: a server's on its first start, a console's the first time
// LinkUp is turned on.
func NewKeyPair() (KeyPair, error) {
	k, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: k.Private, Public: k.Public}, nil
}

// ---- Pairing: Noise_XXpsk0, with the slow hash as the pre-shared key ----

func pairState(psk []byte, static KeyPair, initiator bool) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{
		CipherSuite:           suite,
		Random:                rand.Reader,
		Pattern:               noise.HandshakeXX,
		Initiator:             initiator,
		Prologue:              pairPrologue,
		StaticKeypair:         static.dh(),
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
	})
}

// Joiner is the side with a code: a console, or a server linking.
type Joiner struct {
	hs        *noise.HandshakeState
	recv      *noise.CipherState
	serverKey []byte
}

// NewJoiner starts a pairing, and returns message 1, the joiner's proof.
func NewJoiner(k []byte, static KeyPair) (*Joiner, []byte, error) {
	hs, err := pairState(k, static, true)
	if err != nil {
		return nil, nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return &Joiner{hs: hs}, msg, nil
}

// Second reads message 2, the server's proof, and returns its payload and message 3. A fake server
// cannot make a message 2 that opens, so this is where one is stopped.
func (j *Joiner) Second(msg2, joining []byte) (hello, msg3 []byte, err error) {
	hello, _, _, err = j.hs.ReadMessage(nil, msg2)
	if err != nil {
		return nil, nil, ErrProof
	}
	j.serverKey = j.hs.PeerStatic()
	msg3, _, recv, err := j.hs.WriteMessage(nil, joining)
	if err != nil {
		return nil, nil, err
	}
	j.recv = recv
	return hello, msg3, nil
}

// Reply opens the sealed reply to message 3. Only after this should the server's key be kept.
func (j *Joiner) Reply(sealed []byte) (reply, serverKey []byte, err error) {
	reply, err = j.recv.Decrypt(nil, nil, sealed)
	if err != nil {
		return nil, nil, ErrProof
	}
	return reply, j.serverKey, nil
}

// Responder is the server's side of one pairing.
type Responder struct {
	hs    *noise.HandshakeState
	Index int // which of the keys it was given fitted
	send  *noise.CipherState
	Key   []byte
}

// TryFirst checks message 1 against each waiting code's slow hash. Only the right one opens it, and
// that is cheap, because each hash was worked out when its code was made.
func TryFirst(ks [][]byte, static KeyPair, msg1 []byte) (*Responder, error) {
	for i, k := range ks {
		if len(k) != hashLength {
			// With no key the handshake would run with no code at all, so a damaged file must not.
			continue
		}
		hs, err := pairState(k, static, false)
		if err != nil {
			return nil, err
		}
		if _, _, _, err := hs.ReadMessage(nil, msg1); err == nil {
			return &Responder{hs: hs, Index: i}, nil
		}
	}
	return nil, ErrProof
}

// SecondMessage writes message 2, with the server's hello as its payload.
func (r *Responder) SecondMessage(hello []byte) ([]byte, error) {
	msg, _, _, err := r.hs.WriteMessage(nil, hello)
	return msg, err
}

// Third reads message 3 and returns who is joining.
func (r *Responder) Third(msg3 []byte) ([]byte, error) {
	joining, _, send, err := r.hs.ReadMessage(nil, msg3)
	if err != nil {
		return nil, ErrProof
	}
	r.send = send
	r.Key = r.hs.PeerStatic()
	return joining, nil
}

// SealReply seals the result, under the transport key from responder to initiator, nonce 0.
func (r *Responder) SealReply(reply []byte) ([]byte, error) {
	return r.send.Encrypt(nil, nil, reply)
}

// ---- Sessions: Noise_IK, then sealed requests with explicit counters ----

func sessionState(static KeyPair, peer []byte, initiator bool) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		Prologue:      sessionPrologue,
		StaticKeypair: static.dh(),
		PeerStatic:    peer,
	})
}

// Opening is the side that holds the other's remembered key, between its two messages.
type Opening struct {
	hs *noise.HandshakeState
}

// Open writes the first message of a session.
func Open(static KeyPair, peer, payload []byte) (*Opening, []byte, error) {
	hs, err := sessionState(static, peer, true)
	if err != nil {
		return nil, nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, nil, err
	}
	return &Opening{hs: hs}, msg, nil
}

// Finish reads the answer. A server without the remembered key cannot make one that opens, so a
// fake server at a typed address is stopped here.
func (o *Opening) Finish(msg []byte) ([]byte, *Session, error) {
	payload, send, recv, err := o.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, nil, ErrProof
	}
	return payload, newSession(send, recv), nil
}

// Accepting is the server's side of a session, between its two messages.
type Accepting struct {
	hs *noise.HandshakeState
}

// Accept reads the first message of a session, and returns who it says it is and its payload. The
// caller decides whether that key is known.
func Accept(static KeyPair, msg []byte) (*Accepting, []byte, []byte, error) {
	hs, err := sessionState(static, nil, false)
	if err != nil {
		return nil, nil, nil, err
	}
	payload, _, _, err := hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, nil, nil, ErrProof
	}
	return &Accepting{hs: hs}, hs.PeerStatic(), payload, nil
}

// Answer writes the second message.
func (a *Accepting) Answer(payload []byte) ([]byte, *Session, error) {
	msg, recv, send, err := a.hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, nil, err
	}
	return msg, newSession(send, recv), nil
}

// The two domains a sealed message can be in, so an answer cannot pass for a request.
const (
	domainRequest = 4
	domainAnswer  = 5
)

// Session is one side's keys after the handshake.
type Session struct {
	ID   [SessionIDSize]byte
	send noise.Cipher
	recv noise.Cipher
	next uint64 // the initiator's next counter

	highest uint64 // the responder's window over counters it has seen
	seen    uint64
}

func newSession(send, recv *noise.CipherState) *Session {
	return &Session{send: send.Cipher(), recv: recv.Cipher(), next: 1}
}

func header(domain byte, id [SessionIDSize]byte, counter uint64) []byte {
	ad := make([]byte, 1+SessionIDSize+8)
	ad[0] = domain
	copy(ad[1:], id[:])
	binary.BigEndian.PutUint64(ad[1+SessionIDSize:], counter)
	return ad
}

// SealRequest is the initiator sealing one request: session | counter | ciphertext.
func (s *Session) SealRequest(plaintext []byte) (body []byte, counter uint64) {
	counter = s.next
	s.next++
	ad := header(domainRequest, s.ID, counter)
	body = append(body, ad[1:]...)
	return s.send.Encrypt(body, counter, ad, plaintext), counter
}

// OpenAnswer is the initiator opening the answer to one of its requests.
func (s *Session) OpenAnswer(counter uint64, ciphertext []byte) ([]byte, error) {
	out, err := s.recv.Decrypt(nil, counter, header(domainAnswer, s.ID, counter), ciphertext)
	if err != nil {
		return nil, ErrProof
	}
	return out, nil
}

// ParseSealed splits a sealed body into its session id, counter and ciphertext.
func ParseSealed(body []byte) (id [SessionIDSize]byte, counter uint64, ciphertext []byte, ok bool) {
	if len(body) < SessionIDSize+8+chacha20poly1305.Overhead {
		return id, 0, nil, false
	}
	copy(id[:], body)
	return id, binary.BigEndian.Uint64(body[SessionIDSize:]), body[SessionIDSize+8:], true
}

// OpenRequest is the responder opening a request. It refuses a counter it has seen, or one older
// than the last 64. The counter is marked only once the request has opened, so someone with no key
// cannot use up a counter.
func (s *Session) OpenRequest(counter uint64, ciphertext []byte) ([]byte, error) {
	if counter == 0 || counter > ^uint64(0)-1 || !s.fresh(counter) {
		return nil, ErrProof
	}
	out, err := s.recv.Decrypt(nil, counter, header(domainRequest, s.ID, counter), ciphertext)
	if err != nil {
		return nil, ErrProof
	}
	s.mark(counter)
	return out, nil
}

func (s *Session) fresh(counter uint64) bool {
	if counter > s.highest {
		return true
	}
	back := s.highest - counter
	return back < 64 && s.seen&(1<<back) == 0
}

func (s *Session) mark(counter uint64) {
	if counter > s.highest {
		shift := counter - s.highest
		if shift >= 64 {
			s.seen = 0
		} else {
			s.seen <<= shift
		}
		s.highest = counter
		s.seen |= 1
		return
	}
	s.seen |= 1 << (s.highest - counter)
}

// SealAnswer is the responder sealing the one answer to a request.
func (s *Session) SealAnswer(counter uint64, plaintext []byte) []byte {
	return s.send.Encrypt(nil, counter, header(domainAnswer, s.ID, counter), plaintext)
}

// ---- Between two consoles: Noise_K, one way ----

func boxState(static KeyPair, peer []byte, invite string, initiator bool) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeK,
		Initiator:     initiator,
		Prologue:      []byte(boxPrologue + invite),
		StaticKeypair: static.dh(),
		PeerStatic:    peer,
	})
}

// SealBox seals something for another console, for one invite. The server only carries it. The
// console's own program does this; here it is for the stand-in console and the tests.
func SealBox(static KeyPair, peer []byte, invite string, payload []byte) ([]byte, error) {
	hs, err := boxState(static, peer, invite, true)
	if err != nil {
		return nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, payload)
	return msg, err
}

// OpenBox opens what a console sealed. It fails for a different invite, a different sender or
// anyone but the console it was sealed for.
func OpenBox(static KeyPair, peer []byte, invite string, box []byte) ([]byte, error) {
	hs, err := boxState(static, peer, invite, false)
	if err != nil {
		return nil, err
	}
	out, _, _, err := hs.ReadMessage(nil, box)
	if err != nil {
		return nil, ErrProof
	}
	return out, nil
}
