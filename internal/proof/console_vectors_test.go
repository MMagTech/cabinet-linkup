package proof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/flynn/noise"
)

// TestConsoleVectors writes what the console's own Noise is tested against (linkup/proof-vectors.txt
// in MMagTech/cabinetos), when LINKUP_WRITE_VECTORS names a file. The server's side of each exchange
// is this package's real code, and the console's side has its short-lived keys fixed, so the console's
// test has to come out byte for byte what this code accepted. With nothing set it still runs both sides.
func TestConsoleVectors(t *testing.T) {
	seed := func(name string) []byte {
		sum := sha256.Sum256([]byte("linkup console vectors " + name))
		return sum[:]
	}
	fixed := func(name string) noise.DHKey {
		k, err := noise.DH25519.GenerateKeypair(bytes.NewReader(seed(name)))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	pair := func(name string) KeyPair {
		k := fixed(name)
		return KeyPair{Private: k.Private, Public: k.Public}
	}

	var out strings.Builder
	out.WriteString("# What the console's Noise is held to: the answers of the server's own code, internal/proof in\n")
	out.WriteString("# MMagTech/cabinet-linkup. Written by its TestConsoleVectors (LINKUP_WRITE_VECTORS=<this file> go\n")
	out.WriteString("# test ./internal/proof -run TestConsoleVectors). Throwaway keys, made for this file. Hex.\n\n")
	line := func(k string, v any) {
		switch x := v.(type) {
		case []byte:
			fmt.Fprintf(&out, "%s=%s\n", k, hex.EncodeToString(x))
		default:
			fmt.Fprintf(&out, "%s=%v\n", k, x)
		}
	}

	console := pair("console")
	server := pair("server")
	friend := pair("friend")

	// A pairing: the console's messages made with a fixed key, read by the real responder.
	code := "K7MR4QXT9HPV"
	k := SlowHash(code)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Random: bytes.NewReader(seed("pair ephemeral")), Pattern: noise.HandshakeXX,
		Initiator: true, Prologue: pairPrologue, StaticKeypair: console.dh(), PresharedKey: k, PresharedKeyPlacement: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := TryFirst([][]byte{k}, server, msg1)
	if err != nil {
		t.Fatal(err)
	}
	hello := []byte(`{"protocol":1,"oldest":1,"name":"Riley's"}`)
	msg2, err := r.SecondMessage(hello)
	if err != nil {
		t.Fatal(err)
	}
	gotHello, _, _, err := hs.ReadMessage(nil, msg2)
	if err != nil || !bytes.Equal(gotHello, hello) {
		t.Fatal("hello", err)
	}
	joining := []byte(`{"kind":"console","protocol":1,"oldest":1,"stream":1,"account":"a stable id","username":"alex"}`)
	msg3, _, recv, err := hs.WriteMessage(nil, joining)
	if err != nil {
		t.Fatal(err)
	}
	gotJoining, err := r.Third(msg3)
	if err != nil || !bytes.Equal(gotJoining, joining) {
		t.Fatal("joining", err)
	}
	if !bytes.Equal(r.Key, console.Public) {
		t.Fatal("the responder read the wrong key")
	}
	replyPlain := []byte(`{"ok":true,"console":3}`)
	reply, err := r.SealReply(replyPlain)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := recv.Decrypt(nil, nil, reply); err != nil || !bytes.Equal(got, replyPlain) {
		t.Fatal("reply", err)
	}
	line("code", code)
	line("k", k)
	line("console_private", console.Private)
	line("console_ephemeral", fixed("pair ephemeral").Private)
	line("server_public", server.Public)
	line("msg1", msg1)
	line("msg2", msg2)
	line("hello", hello)
	line("joining", joining)
	line("msg3", msg3)
	line("reply", reply)
	line("reply_plain", replyPlain)
	out.WriteString("\n")

	// A session: an Open read by the real Accept, a request opened by the real session.
	hs, err = noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Random: bytes.NewReader(seed("open ephemeral")), Pattern: noise.HandshakeIK,
		Initiator: true, Prologue: sessionPrologue, StaticKeypair: console.dh(), PeerStatic: server.Public,
	})
	if err != nil {
		t.Fatal(err)
	}
	openPayload := []byte(`{"protocol":1,"oldest":1,"stream":1,"t":1762468800000000000}`)
	open, _, _, err := hs.WriteMessage(nil, openPayload)
	if err != nil {
		t.Fatal(err)
	}
	acc, who, got, err := Accept(server, open)
	if err != nil || !bytes.Equal(who, console.Public) || !bytes.Equal(got, openPayload) {
		t.Fatal("open", err)
	}
	answerPayload := []byte(`{"session":"0011223344556677","protocol":1,"oldest":1}`)
	answer, srv, err := acc.Answer(answerPayload)
	if err != nil {
		t.Fatal(err)
	}
	gotAnswer, send, recv2, err := hs.ReadMessage(nil, answer)
	if err != nil || !bytes.Equal(gotAnswer, answerPayload) {
		t.Fatal("answer", err)
	}
	id := [SessionIDSize]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
	srv.ID = id
	client := newSession(send, recv2)
	client.ID = id
	client.next = 258 // far enough along to show which way round the counter's bytes go
	reqPlain := []byte(`{"op":"friends"}`)
	req, counter := client.SealRequest(reqPlain)
	gotID, gotCounter, cipher, ok := ParseSealed(req)
	if !ok || gotID != id || gotCounter != counter {
		t.Fatal("parse")
	}
	plain, err := srv.OpenRequest(gotCounter, cipher)
	if err != nil || !bytes.Equal(plain, reqPlain) {
		t.Fatal("request", err)
	}
	ansPlain := []byte(`{"friends":[]}`)
	ans := srv.SealAnswer(gotCounter, ansPlain)
	if got, err := client.OpenAnswer(counter, ans); err != nil || !bytes.Equal(got, ansPlain) {
		t.Fatal("reply", err)
	}
	line("console_private", console.Private)
	line("console_ephemeral", fixed("open ephemeral").Private)
	line("server_public", server.Public)
	line("open_payload", openPayload)
	line("open", open)
	line("answer", answer)
	line("answer_payload", answerPayload)
	line("session", id[:])
	line("counter", counter)
	line("request_plain", reqPlain)
	line("request", req)
	line("reply", ans)
	line("reply_plain", ansPlain)
	out.WriteString("\n")

	// Boxes, both ways.
	invite := "00112233445566778899aabbccddeeff"
	fromFriendPlain := []byte(`{"type":"invite","n":1,"expires":1762470600,"from":"maya","to":"alex","stream":1,"game":"Contra III"}`)
	fromFriend, err := SealBox(friend, console.Public, invite, fromFriendPlain)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenBox(console, friend.Public, invite, fromFriend); err != nil || !bytes.Equal(got, fromFriendPlain) {
		t.Fatal("box", err)
	}
	hs, err = noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Random: bytes.NewReader(seed("box ephemeral")), Pattern: noise.HandshakeK, Initiator: true,
		Prologue: []byte(boxPrologue + invite), StaticKeypair: console.dh(), PeerStatic: friend.Public,
	})
	if err != nil {
		t.Fatal(err)
	}
	toFriendPlain := []byte(`{"type":"signal","n":1,"expires":1762470600,"from":"alex","to":"maya","details":"{}"}`)
	toFriend, _, _, err := hs.WriteMessage(nil, toFriendPlain)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenBox(friend, console.Public, invite, toFriend); err != nil || !bytes.Equal(got, toFriendPlain) {
		t.Fatal("box", err)
	}
	line("console_private", console.Private)
	line("friend_private", friend.Private)
	line("invite", invite)
	line("from_friend", fromFriend)
	line("from_friend_plain", fromFriendPlain)
	line("console_ephemeral", fixed("box ephemeral").Private)
	line("to_friend_plain", toFriendPlain)
	line("to_friend", toFriend)

	if path := os.Getenv("LINKUP_WRITE_VECTORS"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
