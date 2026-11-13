package console

// Friend is one line of a person's list. The owner's notes are never in it.
type Friend struct {
	ID       string        `json:"id"`
	Username string        `json:"username"`
	Server   string        `json:"server"`
	Status   string        `json:"status"`
	Key      []byte        `json:"key"`
	Stream   int           `json:"stream"`
	Invite   *FriendInvite `json:"invite,omitempty"`
}

// FriendInvite is an invite from this friend, waiting for an answer.
type FriendInvite struct {
	ID     string `json:"id"`
	Sealed string `json:"sealed"`
}

// Friends is the current account's list.
func (h *Hub) Friends(number int) ([]Friend, error) {
	st := h.store.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.liveFor(number, nil)
	if l.person == "" {
		return nil, ErrNoAccount
	}
	out := []Friend{}
	size := 0
	withBox := func(i *FriendInvite) *FriendInvite {
		if i == nil {
			return nil
		}
		if size += len(i.Sealed); size > maxFriendsBytes {
			i.Sealed = ""
		}
		return i
	}
	for _, id := range st.FriendsOf(l.person) {
		p := st.Person(id)
		if p == nil {
			continue
		}
		at := h.keyConsoleLocked(p)
		f := Friend{ID: id, Username: p.Username, Server: st.Settings.Name, Status: h.statusLocked(&st, id)}
		if c := st.ConsoleByNumber(at); c != nil {
			f.Key = c.Key
		}
		if pl := h.lives[at]; pl != nil {
			f.Stream = pl.stream
		}
		f.Invite = withBox(h.inviteFromLocked(id, l.person, number))
		out = append(out, f)
	}
	for _, link := range st.Links {
		for _, pair := range link.Pairs {
			if pair.Ours != l.person {
				continue
			}
			f := Friend{ID: "p:" + pair.ID, Username: pair.TheirName, Server: link.Name, Status: StatusOffline, Key: pair.TheirKey, Stream: pair.TheirStream}
			if r := h.remotes[link.ID][pair.Theirs]; r != nil && !h.behind[link.ID] {
				f.Status = r.status
			}
			f.Invite = withBox(h.inviteFromLocked(pair.Theirs, l.person, number))
			out = append(out, f)
		}
	}
	return out, nil
}

// inviteFromLocked is a knock from one person to another that rings this console.
func (h *Hub) inviteFromLocked(from, to string, number int) *FriendInvite {
	for _, inv := range h.invites {
		if inv.from == from && inv.to == to && inv.toConsole == number && !inv.joined {
			return &FriendInvite{ID: inv.id, Sealed: inv.sealed}
		}
	}
	return nil
}
