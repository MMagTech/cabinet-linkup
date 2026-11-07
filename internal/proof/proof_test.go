package proof

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func keys(t *testing.T) (KeyPair, KeyPair) {
	t.Helper()
	a, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestKnownSlowHash(t *testing.T) {
	want := "c3c7e78d316b58eff025cd98473fc042a0ebd4ff209be27d491583c495e8b6ad"
	if got := hex.EncodeToString(SlowHash("K7MR4QXT9HPV")); got != want {
		t.Errorf("%s: PROTOCOL.md gives this value for the console's side to check against", got)
	}
}

func TestSaltIsSixteenBytes(t *testing.T) {
	if len(hashSalt) != 16 {
		t.Errorf("%d bytes: libsodium takes exactly 16", len(hashSalt))
	}
}

func TestCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := NewCode()
		if len(c) != 14 || c[4] != '-' || c[9] != '-' {
			t.Fatal(c)
		}
		for _, r := range strings.ReplaceAll(c, "-", "") {
			if !strings.ContainsRune(codeLetters, r) {
				t.Fatalf("%c is not a code letter", r)
			}
		}
		tidy, ok := TidyCode(c)
		if !ok || len(tidy) != 12 {
			t.Fatal(tidy)
		}
		seen[c] = true
	}
	if len(seen) != 200 {
		t.Error("codes repeat")
	}
	for _, typed := range []string{"k7mr 4qxt-9hpv", "K7MR4QXT9HPV", " k7mr-4qxt-9hpv "} {
		if got, ok := TidyCode(typed); !ok || got != "K7MR4QXT9HPV" {
			t.Errorf("%q: %q %v", typed, got, ok)
		}
	}
	for _, typed := range []string{"", "K7MR", "K7MR4QXT9HPVX", "0O1IL0O1IL0O"} {
		if _, ok := TidyCode(typed); ok {
			t.Errorf("%q should not be a code", typed)
		}
	}
	if len(codeLetters) != 28 {
		t.Error("PROTOCOL.md gives 28")
	}
	for _, r := range codeLetters {
		if !strings.ContainsRune(codeAccepts, r) {
			t.Errorf("%c is drawn but not accepted", r)
		}
	}
}

func pair(t *testing.T, joinK, serverK []byte) (*Joiner, *Responder, []byte, error) {
	t.Helper()
	joinKey, serverKey := keys(t)
	j, msg1, err := NewJoiner(joinK, joinKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg1) != 48 {
		t.Fatalf("message 1 is %d bytes", len(msg1))
	}
	r, err := TryFirst([][]byte{serverK}, serverKey, msg1)
	return j, r, msg1, err
}

func TestPairing(t *testing.T) {
	k := SlowHash("K7MR4QXT9HPV")
	joinKey, serverKey := keys(t)
	j, msg1, _ := NewJoiner(k, joinKey)
	other := SlowHash("T3WN8FJY6CRE")
	r, err := TryFirst([][]byte{other, k}, serverKey, msg1)
	if err != nil || r.Index != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	msg2, err := r.SecondMessage([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	hello, msg3, err := j.Second(msg2, []byte("joining"))
	if err != nil || string(hello) != "hello" {
		t.Fatalf("%q %v", hello, err)
	}
	joining, err := r.Third(msg3)
	if err != nil || string(joining) != "joining" || !bytes.Equal(r.Key, joinKey.Public) {
		t.Fatalf("%q %v", joining, err)
	}
	sealed, err := r.SealReply([]byte("ok"))
	if err != nil {
		t.Fatal(err)
	}
	reply, key, err := j.Reply(sealed)
	if err != nil || string(reply) != "ok" || !bytes.Equal(key, serverKey.Public) {
		t.Fatalf("%q %v", reply, err)
	}
}

func TestADamagedKeyNeverMatches(t *testing.T) {
	joinKey, serverKey := keys(t)
	_, msg1, err := pairStateFor(joinKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range [][]byte{nil, {}, make([]byte, 16)} {
		if _, err := TryFirst([][]byte{k}, serverKey, msg1); err == nil {
			t.Errorf("a key of %d bytes must not match", len(k))
		}
	}
}

func pairStateFor(k KeyPair) (*Joiner, []byte, error) {
	return NewJoiner(SlowHash("K7MR4QXT9HPV"), k)
}

func TestWrongCodeIsRefused(t *testing.T) {
	if _, _, _, err := pair(t, SlowHash("K7MR4QXT9HPV"), SlowHash("T3WN8FJY6CRE")); err == nil {
		t.Error("message 1 under a different code must not open")
	}
}

// A fake server has its own idea of K: it cannot make a message 2 the joiner can open.
func TestFakeServerAnswerIsRefused(t *testing.T) {
	realK := SlowHash("K7MR4QXT9HPV")
	joinKey, fakeKey := keys(t)
	j, msg1, _ := NewJoiner(realK, joinKey)
	// The fake answers with a handshake of its own, under a guess, having read nothing from message 1.
	guess := SlowHash("T3WN8FJY6CRE")
	fakeJoiner, fake1, _ := NewJoiner(guess, fakeKey)
	_ = fakeJoiner
	r, err := TryFirst([][]byte{guess}, fakeKey, fake1)
	if err != nil {
		t.Fatal(err)
	}
	msg2, _ := r.SecondMessage(nil)
	if _, _, err := j.Second(msg2, nil); err == nil {
		t.Error("a fake server's message 2 must not open")
	}
	_ = msg1
}

// Someone swapping a key in the middle, without K, breaks the handshake.
func TestTamperedMessagesFail(t *testing.T) {
	k := SlowHash("K7MR4QXT9HPV")
	joinKey, serverKey := keys(t)
	j, msg1, _ := NewJoiner(k, joinKey)
	bad := bytes.Clone(msg1)
	bad[0] ^= 1
	if _, err := TryFirst([][]byte{k}, serverKey, bad); err == nil {
		t.Error("a changed message 1")
	}
	r, err := TryFirst([][]byte{k}, serverKey, msg1)
	if err != nil {
		t.Fatal(err)
	}
	msg2, _ := r.SecondMessage(nil)
	bad = bytes.Clone(msg2)
	bad[40] ^= 1
	if _, _, err := j.Second(bad, nil); err == nil {
		t.Error("a changed message 2")
	}
}

func openedPair(t *testing.T) (client, server *Session, clientKey, serverKey KeyPair) {
	t.Helper()
	clientKey, serverKey = keys(t)
	o, msg, err := Open(clientKey, serverKey.Public, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	a, who, payload, err := Accept(serverKey, msg)
	if err != nil || !bytes.Equal(who, clientKey.Public) || string(payload) != "hi" {
		t.Fatalf("%v %q", err, payload)
	}
	answer, server, err := a.Answer([]byte("yes"))
	if err != nil {
		t.Fatal(err)
	}
	said, client, err := o.Finish(answer)
	if err != nil || string(said) != "yes" {
		t.Fatalf("%v %q", err, said)
	}
	return client, server, clientKey, serverKey
}

func TestSealedRequests(t *testing.T) {
	c, s, _, _ := openedPair(t)
	body, counter := c.SealRequest([]byte("request"))
	if counter != 1 {
		t.Errorf("counters start at 1, got %d", counter)
	}
	_, n, ct, ok := ParseSealed(body)
	if !ok || n != counter {
		t.Fatal(ok, n)
	}
	plain, err := s.OpenRequest(n, ct)
	if err != nil || string(plain) != "request" {
		t.Fatalf("%q %v", plain, err)
	}
	if _, err := s.OpenRequest(n, ct); err == nil {
		t.Error("a repeat must be refused")
	}
	answer := s.SealAnswer(n, []byte("answer"))
	out, err := c.OpenAnswer(n, answer)
	if err != nil || string(out) != "answer" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := c.OpenAnswer(n+1, answer); err == nil {
		t.Error("an answer cannot be moved to another request")
	}
}

func TestAnAnswerCannotPassForARequest(t *testing.T) {
	c, s, _, _ := openedPair(t)
	body, n := c.SealRequest([]byte("x"))
	_, _, ct, _ := ParseSealed(body)
	s.OpenRequest(n, ct)
	answer := s.SealAnswer(n, []byte("y"))
	if _, err := s.OpenRequest(n+1, answer); err == nil {
		t.Error("an answer passed for a request")
	}
}

func TestWindow(t *testing.T) {
	c, s, _, _ := openedPair(t)
	var bodies [][]byte
	for range 100 {
		b, _ := c.SealRequest([]byte("x"))
		bodies = append(bodies, b)
	}
	open := func(i int) error {
		_, n, ct, _ := ParseSealed(bodies[i])
		_, err := s.OpenRequest(n, ct)
		return err
	}
	if err := open(90); err != nil {
		t.Fatal(err)
	}
	if err := open(50); err != nil {
		t.Errorf("out of order inside the window: %v", err)
	}
	if err := open(10); err == nil {
		t.Error("older than the window")
	}
	if err := open(50); err == nil {
		t.Error("a repeat inside the window")
	}
	if err := open(99); err != nil {
		t.Error(err)
	}
}

func TestBadCountersAndBadBytesChangeNothing(t *testing.T) {
	c, s, _, _ := openedPair(t)
	body, n := c.SealRequest([]byte("x"))
	_, _, ct, _ := ParseSealed(body)
	if _, err := s.OpenRequest(0, ct); err == nil {
		t.Error("counter 0")
	}
	if _, err := s.OpenRequest(^uint64(0), ct); err == nil {
		t.Error("the top counter")
	}
	bad := bytes.Clone(ct)
	bad[0] ^= 1
	if _, err := s.OpenRequest(n, bad); err == nil {
		t.Error("a changed byte")
	}
	if _, err := s.OpenRequest(n+1000, ct); err == nil {
		t.Error("a counter that does not fit its bytes")
	}
	if s.highest != 0 || s.seen != 0 {
		t.Error("a failed request must change nothing")
	}
	if _, err := s.OpenRequest(n, ct); err != nil {
		t.Errorf("the real one still works: %v", err)
	}
}

func TestSessionHasItsOwnKeys(t *testing.T) {
	c1, s1, ck, sk := openedPair(t)
	o, msg, _ := Open(ck, sk.Public, nil)
	a, _, _, _ := Accept(sk, msg)
	answer, s2, _ := a.Answer(nil)
	_, c2, _ := o.Finish(answer)
	body, n := c1.SealRequest([]byte("x"))
	_, _, ct, _ := ParseSealed(body)
	if _, err := s2.OpenRequest(n, ct); err == nil {
		t.Error("one session's request opened another's")
	}
	if _, err := s1.OpenRequest(n, ct); err != nil {
		t.Error(err)
	}
	_ = c2
}

func TestOpenToTheWrongKeyFails(t *testing.T) {
	ck, sk := keys(t)
	_, wrong := keys(t)
	_, msg, _ := Open(ck, wrong.Public, nil)
	if _, _, _, err := Accept(sk, msg); err == nil {
		t.Error("a message to another key must not open")
	}
}

func TestAnswerFromTheWrongServerFails(t *testing.T) {
	ck, sk := keys(t)
	o, _, _ := Open(ck, sk.Public, nil)
	_, fake := keys(t)
	oFake, msg, _ := Open(ck, fake.Public, nil)
	_ = oFake
	a, _, _, err := Accept(fake, msg)
	if err != nil {
		t.Fatal(err)
	}
	answer, _, _ := a.Answer(nil)
	if _, _, err := o.Finish(answer); err == nil {
		t.Error("a server without the remembered key must not pass")
	}
}

func TestBoxes(t *testing.T) {
	a, b := keys(t)
	_, stranger := keys(t)
	box, err := SealBox(a, b.Public, "invite1", []byte("Contra III"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(box, []byte("Contra")) {
		t.Error("readable")
	}
	out, err := OpenBox(b, a.Public, "invite1", box)
	if err != nil || string(out) != "Contra III" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := OpenBox(b, a.Public, "invite2", box); err == nil {
		t.Error("bound to one invite")
	}
	if _, err := OpenBox(b, stranger.Public, "invite1", box); err == nil {
		t.Error("bound to its sender")
	}
	if _, err := OpenBox(stranger, a.Public, "invite1", box); err == nil {
		t.Error("only for the console it was sealed for")
	}
}

func TestBases(t *testing.T) {
	for in, want := range map[string][]string{
		"linkup.example.net:7845":         {"http://linkup.example.net:7845"},
		"203.0.113.5:7845":                {"http://203.0.113.5:7845"},
		"[2001:db8::1]:7845":              {"http://[2001:db8::1]:7845"},
		"linkup.example.net":              {"https://linkup.example.net", "http://linkup.example.net"},
		"https://proxy.example.net/games": {"https://proxy.example.net/games"},
		"http://proxy.example.net:8080/":  {"http://proxy.example.net:8080"},
		"proxy.example.net/games":         {"https://proxy.example.net/games", "http://proxy.example.net/games"},
		"  name.tailnet.ts.net:7845 ":     {"http://name.tailnet.ts.net:7845"},
	} {
		got := Bases(in)
		if len(got) != len(want) {
			t.Errorf("%q: %v", in, got)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: %v", in, got)
			}
		}
	}
}

func TestPushedAddresses(t *testing.T) {
	for _, ok := range []string{"linkup.example.net:7845", "203.0.113.5:7845", "[2001:db8::1]:7845", "linkup.example.net", "https://proxy.example.net/games", "http://x.example:1/a/b"} {
		if !PushedAddressOK(ok) {
			t.Errorf("%q is fine", ok)
		}
	}
	for _, bad := range []string{"", "http://192.168.1.1/a?x=", "x#y", "x/reboot%3Fnow%3D1", "x/a%00", "user@host", "ftp://host", "file:///etc", "a b", "a\nb", "gopher://x", `a\b`, "https://", "//x", strings.Repeat("x", 300)} {
		if PushedAddressOK(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}
