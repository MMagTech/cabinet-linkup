package console

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/store"
)

func TestANewAccountAppearsWithNoFriends(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "parent")
	id, _, err := r.h.Signin(1, "second", "second", StatusOnline, false)
	if err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if p := st.Person(id); p == nil || p.Username != "second" || p.LastSeen.IsZero() {
		t.Fatalf("%+v", p)
	}
	if list, _ := r.h.Friends(1); len(list) != 0 {
		t.Errorf("%+v", list)
	}
}

func TestSameAccountKeepsItsPersonAndLastUsername(t *testing.T) {
	r := newHubRig(t)
	id := r.console(1, "alex")
	again, _, err := r.h.Signin(1, "alex", "alexander", StatusOnline, false)
	if err != nil || again != id {
		t.Fatalf("%v %v", again, err)
	}
	st := r.st.Snapshot()
	if st.Person(id).Username != "alexander" {
		t.Error("the server keeps the last username it was told")
	}
}

func TestSigninRefusesNonsense(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "a")
	for _, c := range [][3]string{{"", "x", ""}, {"x", "", ""}, {"x", "bad\nname", ""}, {"x", "y", "weird"}} {
		if _, _, err := r.h.Signin(1, c[0], c[1], c[2], false); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%q: %v", c, err)
		}
	}
	for i := range peoplePerConsole + 2 {
		r.h.Signin(1, string(rune('A'+i)), "n", StatusOnline, false)
	}
	st := r.st.Snapshot()
	if len(st.People) > peoplePerConsole {
		t.Errorf("%d people on one console", len(st.People))
	}
}

func TestStatusShowsOfflineForEveryReason(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	if got := statusOf(t, r.h, 1, b); got != StatusOnline {
		t.Fatal(got)
	}

	r.h.SetStatus(2, StatusOffline, false)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("appearing offline")
	}
	r.h.SetStatus(2, StatusOnline, false)
	if got := statusOf(t, r.h, 1, b); got != StatusOnline {
		t.Error("one press brings them back")
	}

	other, _, _ := r.h.Signin(2, "other", "other", StatusOnline, false)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("someone else signed in on their console")
	}
	if !r.h.Online(other) || r.h.Online(b) {
		t.Error("the page must agree")
	}
	r.h.Signin(2, "b", "b", StatusOnline, false)

	r.h.Signout(2)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("signed out")
	}
	r.h.Signin(2, "b", "b", StatusOnline, false)

	r.advance(consoleSilence + time.Second)
	r.h.Seen(1)
	if got := statusOf(t, r.h, 1, b); got != StatusOffline {
		t.Error("a console that stopped asking")
	}
}

func TestStatusIsOneAccountsOwn(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "parent")
	alex := r.console(2, "alex")
	r.friends(r.st.Snapshot().People[0].ID, alex)
	r.h.SetStatus(2, StatusOffline, false)
	kid, _, _ := r.h.Signin(2, "kid", "kid", StatusOnline, false)
	if !r.h.Online(kid) {
		t.Error("one account appearing offline holds for it and not another")
	}
}

func TestNoEventForComingOnline(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	r.friends(a, b)
	r.events(1)
	r.h.SetStatus(2, StatusOffline, false)
	r.h.SetStatus(2, StatusOnline, false)
	for _, e := range r.events(1) {
		if e.Type != "friends" || e.Invite != "" || e.From != "" {
			t.Errorf("an event must say nothing about who: %+v", e)
		}
	}
}

func TestSigningInAgainDoesNotSave(t *testing.T) {
	r := newHubRig(t)
	r.console(1, "a")
	var before, after store.State
	r.st.ReadJSON("state.json", &before)
	before.Games.Direct = 12345
	r.st.WriteJSON("state.json", &before)
	r.h.Signin(1, "a", "a", StatusOnline, false)
	r.st.ReadJSON("state.json", &after)
	if after.Games.Direct != 12345 {
		t.Error("nothing changed, so nothing should be written")
	}
}

// An app that only joins says so at pairing and at sign-in. An invite made for someone who only
// joins brings in someone else who only joins with no friendship, since neither could host, and
// someone with a console as their friend.
func TestTwoWhoOnlyJoinAreNeverMadeFriends(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	no := false
	invite := func(forPerson string) store.Code {
		return store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: forPerson, Expires: r.clock.Add(time.Hour)}
	}
	pair := func(code store.Code, number int, account string) string {
		t.Helper()
		r.st.Update(func(st *store.State) error {
			st.Codes = append(st.Codes, code)
			st.NextConsole = number
			return nil
		})
		if _, err := r.h.joinConsole(code, []byte{byte(number)}, consoleJoining{Account: account, Username: account, Hosts: &no}); err != nil {
			t.Fatal(err)
		}
		st := r.st.Snapshot()
		return st.PersonOn(number, account).ID
	}
	riley := pair(invite(me), 2, "riley")
	sam := pair(invite(riley), 3, "sam")
	st := r.st.Snapshot()
	if !st.Person(riley).JoinsOnly || !st.Person(sam).JoinsOnly || st.Person(me).JoinsOnly {
		t.Errorf("only the two on 2P Start are marked: %+v", st.People)
	}
	if !st.AreFriends(riley, me) || st.AreFriends(sam, riley) {
		t.Errorf("riley is my friend, sam is not riley's: %+v", st.Friends)
	}
	if st.CodeOf(sam) == nil {
		t.Error("sam is in all the same, with the invite as their own code")
	}
}

// A 2P Start paired before it said so is marked at its next sign-in, and a sign-in that does not
// say it is an app that hosts.
func TestSigninSaysWhetherItHosts(t *testing.T) {
	r := newHubRig(t)
	id := r.console(1, "riley")
	if _, _, err := r.h.Signin(1, "riley", "riley", StatusOnline, true); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if !st.Person(id).JoinsOnly {
		t.Error("marked at sign-in")
	}
	r.h.Signin(1, "riley", "riley", StatusOnline, false)
	st = r.st.Snapshot()
	if st.Person(id).JoinsOnly {
		t.Error("a sign-in with no hosts is one that hosts")
	}
	newcomer, _, _ := r.h.Signin(1, "sam", "sam", StatusOnline, true)
	st = r.st.Snapshot()
	if !st.Person(newcomer).JoinsOnly {
		t.Error("someone new at sign-in is marked too")
	}
}

// ---- One person on more than one console ----

// join pairs a new console with an invite made for someone, as a console using the code does, and
// signs that account in on it.
func (r *hubRig) join(number int, forPerson, account string) string {
	r.t.Helper()
	code := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: forPerson, Expires: r.clock.Add(time.Hour)}
	r.st.Update(func(st *store.State) error {
		st.Codes = append(st.Codes, code)
		st.NextConsole = number
		return nil
	})
	if res, err := r.h.joinConsole(code, []byte{byte(number)}, consoleJoining{Account: account, Username: account}); err != nil || res.Console != number {
		r.t.Fatalf("%+v %v", res, err)
	}
	r.h.Connected(number, []byte{byte(number)}, 1)
	id, _, err := r.h.Signin(number, account, account, StatusOnline, false)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func keyOf(t *testing.T, h *Hub, number int, friend string) []byte {
	t.Helper()
	list, _ := h.Friends(number)
	for _, f := range list {
		if f.ID == friend {
			return f.Key
		}
	}
	t.Fatalf("%s is not on the list", friend)
	return nil
}

func hasChosen(events []Event, status string) bool {
	for _, e := range events {
		if e.Type == "chosen" && e.Status == status {
			return true
		}
	}
	return false
}

// me on consoles 1 and 2, 2 used last, and a friend on console 3.
func twoConsoles(t *testing.T) (*hubRig, string, string) {
	r := newHubRig(t)
	me := r.console(1, "me")
	friend := r.console(3, "friend")
	r.friends(me, friend)
	r.advance(time.Second)
	if again := r.join(2, me, "me"); again != me {
		t.Fatalf("console 2 made a second me: %s", again)
	}
	r.events(1)
	r.events(2)
	r.events(3)
	return r, me, friend
}

func TestAnInviteForSomeoneUsedByThemAddsTheirConsole(t *testing.T) {
	r, me, friend := twoConsoles(t)
	st := r.st.Snapshot()
	if p := st.Person(me); !slices.Equal(p.Consoles(), []int{2, 1}) || len(st.People) != 2 || st.AreFriends(me, me) {
		t.Errorf("%+v", st.People)
	}
	if list, _ := r.h.Friends(3); len(list) != 1 || list[0].ID != me {
		t.Errorf("the friend sees me once: %+v", list)
	}
	for _, n := range []int{1, 2} {
		if list, _ := r.h.Friends(n); len(list) != 1 || list[0].ID != friend {
			t.Errorf("console %d: never in my own list: %+v", n, list)
		}
	}
}

// Console 2 came in with me, so it knows everyone already on my consoles.
func TestAJoinedConsoleKnowsWhoIsOnTheOthers(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	kid, _, _ := r.h.Signin(1, "kid", "kid", StatusOnline, false)
	r.h.Signin(1, "me", "me", StatusOnline, false)
	r.join(2, me, "me")
	if again, _, _ := r.h.Signin(2, "kid", "kid", StatusOnline, false); again != kid {
		t.Errorf("the kid on console 2 is someone new")
	}
}

// An account is not a secret, so an invite used by anyone else only brings in new people.
func TestAnInviteUsedBySomeoneElseOnlyBringsNewPeople(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	danny := r.join(3, me, "danny")
	st := r.st.Snapshot()
	if danny == me || !st.AreFriends(me, danny) || st.Person(me).On(3) {
		t.Errorf("%+v", st.People)
	}
	if other, _, _ := r.h.Signin(3, "me", "me", StatusOnline, false); other == me {
		t.Error("my account on Danny's console is not me")
	}
}

func TestAKnockRingsTheConsoleUsedLast(t *testing.T) {
	r, me, _ := twoConsoles(t)
	if key := keyOf(t, r.h, 3, me); !slices.Equal(key, []byte{2}) {
		t.Errorf("the friend is given console 2's key: %v", key)
	}
	id, err := r.h.Invite(3, newInviteID(), me, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(2), "invite", "") || hasEvent(r.events(1), "invite", "") {
		t.Error("it rings console 2 only")
	}
	if list, _ := r.h.Friends(1); list[0].Invite != nil {
		t.Error("console 1 shows no knock")
	}
	if list, _ := r.h.Friends(2); list[0].Invite == nil {
		t.Error("console 2 does")
	}
	if _, err := r.h.Join(1, id); err == nil {
		t.Error("only the console it rang can answer")
	}
	if _, err := r.h.Join(2, id); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(3), "joined", "") {
		t.Error("the friend hears")
	}
}

// Of the consoles showing me Online, a knock rings the one I came back to last, which is then the
// one used last.
func TestComingBackToAConsoleMovesTheKnock(t *testing.T) {
	r, me, _ := twoConsoles(t)
	r.h.SetStatus(2, StatusOffline, false)
	if statusOf(t, r.h, 3, me) != StatusOnline || !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) {
		t.Error("console 1 still shows me, so it rings there")
	}
	r.h.SetStatus(1, StatusOffline, false)
	if statusOf(t, r.h, 3, me) != StatusOffline {
		t.Error("both are Offline")
	}
	r.advance(time.Second)
	r.h.SetStatus(1, StatusOnline, false)
	st := r.st.Snapshot()
	if !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) || st.Person(me).Console != 1 {
		t.Error("back on console 1, it is the one used last")
	}
}

// The friend opens my box with the key on their list, so while I have an invite out, they are given
// the key of the console it came from.
func TestAnInviteFromTheOtherConsoleGivesItsKey(t *testing.T) {
	r, _, friend := twoConsoles(t)
	id, err := r.h.Invite(1, newInviteID(), friend, "s")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := r.h.Friends(3)
	if !slices.Equal(list[0].Key, []byte{1}) || list[0].Invite == nil || list[0].Invite.ID != id {
		t.Errorf("%+v", list[0])
	}
	if err := r.h.Cancel(2, id); err == nil {
		t.Error("only the console it came from can take it back")
	}
	r.h.Signout(2)
	if r.h.invites[id] == nil {
		t.Error("leaving the other console does not take it back")
	}
}

func TestOfflineChosenOnOneConsoleHoldsOnAll(t *testing.T) {
	r, me, _ := twoConsoles(t)
	r.h.SetStatus(1, StatusOffline, true)
	if statusOf(t, r.h, 3, me) != StatusOffline {
		t.Error("Offline, though console 2 is on")
	}
	if !hasChosen(r.events(2), StatusOffline) || hasChosen(r.events(1), StatusOffline) {
		t.Error("console 2 hears it, console 1 said it")
	}
	_, chose, _ := r.h.Signin(2, "me", "me", StatusOnline, false)
	if st := r.st.Snapshot(); !chose || !st.Person(me).ChoseOffline {
		t.Error("a console signing in is told, and it is saved")
	}
	if _, err := r.h.Invite(3, newInviteID(), me, "s"); err == nil {
		t.Error("nobody can knock")
	}
	r.h.SetStatus(2, StatusOffline, false)
	r.h.SetStatus(2, StatusOnline, true)
	if statusOf(t, r.h, 3, me) != StatusOnline || !hasChosen(r.events(1), StatusOnline) {
		t.Error("Online chosen on console 2 holds too")
	}
	r.h.SetStatus(2, StatusOffline, false)
	if st := r.st.Snapshot(); st.Person(me).ChoseOffline {
		t.Error("a console's own Offline is not a choice")
	}
}

// Only that console goes: what I have open on another carries on.
func TestRemovingOneOfTheirConsoles(t *testing.T) {
	r, me, _ := twoConsoles(t)
	id, _ := r.h.Invite(3, newInviteID(), me, "s")
	r.h.Join(2, id)
	r.st.Update(func(st *store.State) error { st.RemoveConsole(1); return nil })
	r.h.ConsoleRemoved(1)
	if !hasEvent(r.events(1), "removed", "") || r.h.invites[id] == nil || r.h.StatusOf(me) != StatusLinkedUp {
		t.Error("the game on console 2 carries on")
	}
	r.st.Update(func(st *store.State) error { st.RemoveConsole(2); return nil })
	r.h.ConsoleRemoved(2)
	if r.h.invites[id] != nil || len(r.relay.ended) == 0 {
		t.Error("removing console 2 ends it")
	}
}

// A knock ringing a console that is taken off ends, and its inviter hears.
func TestAKnockOnARemovedConsoleIsUnavailable(t *testing.T) {
	r, me, _ := twoConsoles(t)
	id, _ := r.h.Invite(3, newInviteID(), me, "s")
	r.h.ConsoleRemoved(2)
	if r.h.invites[id] != nil || !hasEvent(r.events(3), "invite_ended", EndUnavailable) {
		t.Error("the inviter hears it is unavailable")
	}
}

func TestLeavingOneOfTheirConsoles(t *testing.T) {
	r, me, friend := twoConsoles(t)
	if err := r.h.Leave(2); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if p := st.Person(me); p == nil || !slices.Equal(p.Consoles(), []int{1}) || !st.AreFriends(me, friend) {
		t.Errorf("%+v", st.People)
	}
	if !slices.Equal(keyOf(t, r.h, 3, me), []byte{1}) {
		t.Error("the friend is given console 1's key")
	}
}

func TestAKnockFromALinkRingsTheConsoleUsedLast(t *testing.T) {
	r, _, maya, _ := linkedRig(t)
	r.advance(time.Second)
	r.join(3, maya, "maya")
	r.events(1)
	r.events(3)
	if r.h.KeyConsole(maya) != 3 {
		t.Error("the linked server hears console 3's key")
	}
	id := newInviteID()
	if err := r.h.RemoteInvite("l1", id, "june-there", maya, "s"); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(r.events(3), "invite", "") || hasEvent(r.events(1), "invite", "") {
		t.Error("it rings console 3 only")
	}
	if _, err := r.h.Join(3, id); err != nil {
		t.Fatal(err)
	}
}

// pairWith uses a code on a console, as a console pairing does.
func (r *hubRig) pairWith(code store.Code, number int, account string) error {
	r.st.Update(func(st *store.State) error {
		if st.Code(code.ID) == nil {
			st.Codes = append(st.Codes, code)
		}
		if st.ConsoleByNumber(number) == nil {
			st.NextConsole = number
		}
		return nil
	})
	_, err := r.h.joinConsole(code, []byte{byte(number)}, consoleJoining{Account: account, Username: account})
	return err
}

// A person's code adds any of their consoles, as often as they like, and nobody else's.
func TestAPersonsCodeAddsEachOfTheirConsoles(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	code := store.Code{ID: store.NewID(), Kind: store.CodePerson, For: me}
	for _, n := range []int{2, 3} {
		if err := r.pairWith(code, n, "me"); err != nil {
			t.Fatal(err)
		}
	}
	st := r.st.Snapshot()
	if p := st.Person(me); !slices.Equal(p.Consoles(), []int{3, 2, 1}) || len(st.People) != 1 || st.Code(code.ID) == nil {
		t.Errorf("one person on three consoles, the code kept: %+v", st)
	}
	if err := r.pairWith(code, 4, "someone"); err == nil {
		t.Error("another account is refused")
	}
	if st := r.st.Snapshot(); st.ConsoleByNumber(4) != nil || len(st.People) != 1 {
		t.Errorf("and nothing is made for it: %+v", st)
	}
}

// After Change code, only the new code brings them back on a console that was signed out.
func TestChangeCodeSignsThemOutUntilTheNewCode(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	friend := r.console(3, "friend")
	r.friends(me, friend)
	r.st.Update(func(st *store.State) error { st.SignOut(me); return nil })
	r.h.ConsoleRemoved(1)
	invite := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: friend, Expires: r.clock.Add(time.Hour)}
	if err := r.pairWith(invite, 1, "me"); err != nil {
		t.Fatal(err)
	}
	if st := r.st.Snapshot(); st.Person(me).On(1) {
		t.Error("someone else's invite does not bring them back")
	}
	r.st.Update(func(st *store.State) error {
		st.Consoles[0].Removed = true
		st.People = slices.DeleteFunc(st.People, func(p store.Person) bool { return p.ID != me && p.ID != friend })
		return nil
	})
	code := store.Code{ID: store.NewID(), Kind: store.CodePerson, For: me}
	if err := r.pairWith(code, 1, "me"); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	if !st.Person(me).On(1) || st.ConsoleByNumber(1).Removed || !st.AreFriends(me, friend) {
		t.Errorf("back with their friends: %+v", st)
	}
}

// The invite someone used is their code from then on: the same letters, on each of their consoles,
// with no expiry, and for nobody else.
func TestTheInviteTheyUsedIsTheirCode(t *testing.T) {
	r := newHubRig(t)
	me := r.console(1, "me")
	invite := store.Code{ID: store.NewID(), Kind: store.CodeInvite, For: me, Note: "Danny", Expires: r.clock.Add(time.Hour)}
	if err := r.pairWith(invite, 3, "danny"); err != nil {
		t.Fatal(err)
	}
	st := r.st.Snapshot()
	danny := st.PersonOn(3, "danny")
	c := st.Code(invite.ID)
	if danny == nil || c == nil || c.Kind != store.CodePerson || c.For != danny.ID || !c.Expires.IsZero() || !st.AreFriends(me, danny.ID) {
		t.Fatalf("%+v", st)
	}
	if err := r.pairWith(*c, 4, "danny"); err != nil {
		t.Fatal(err)
	}
	if err := r.pairWith(*c, 5, "someone"); err == nil {
		t.Error("nobody else can use it")
	}
	st = r.st.Snapshot()
	if p := st.Person(danny.ID); !slices.Equal(p.Consoles(), []int{4, 3}) || len(st.People) != 2 {
		t.Errorf("Danny on both his consoles: %+v", st.People)
	}
}
