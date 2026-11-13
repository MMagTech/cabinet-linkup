package console

import (
	"testing"
)

func TestAKidSeesOnlyTheirOwnFriends(t *testing.T) {
	r := newHubRig(t)
	parent := r.console(1, "parent")
	alex := r.console(2, "alex")
	theo := r.console(3, "theo")
	r.friends(parent, alex)
	kid, _, err := r.h.Signin(1, "kid", "kid", StatusOnline, false)
	if err != nil {
		t.Fatal(err)
	}
	r.friends(kid, theo)
	list, _ := r.h.Friends(1)
	if len(list) != 1 || list[0].ID != theo {
		t.Errorf("the kid sees %+v", list)
	}
	r.h.Signin(1, "parent", "parent", StatusOnline, false)
	list, _ = r.h.Friends(1)
	if len(list) != 1 || list[0].ID != alex {
		t.Errorf("the parent sees %+v", list)
	}
}

func TestTwoFriendsOfOnePersonDoNotSeeEachOther(t *testing.T) {
	r := newHubRig(t)
	a := r.console(1, "a")
	b := r.console(2, "b")
	c := r.console(3, "c")
	r.friends(a, b)
	r.friends(a, c)
	list, _ := r.h.Friends(2)
	if len(list) != 1 || list[0].ID != a {
		t.Errorf("%+v", list)
	}
}
