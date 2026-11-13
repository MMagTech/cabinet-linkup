package console

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/MMagTech/cabinet-linkup/internal/proof"
	"github.com/MMagTech/cabinet-linkup/internal/store"
)

const (
	pairingLife     = 60 * time.Second
	maxPairings     = 32
	pairingsPerCode = 4
	pairFirstSize   = 48

	// Failed Pair attempts allowed before a failure answers 429, per source and in total.
	triesPerAddress = 5
	triesInAll      = 30
	triesWindow     = time.Minute
	// Public sources whose failures are kept at once. Past this a new source counts only in the
	// total, as a reverse proxy does, so a guesser with many addresses cannot grow the table.
	triesSources = 4096
	// Codes tried a second, past which nothing more is tried: about a tenth of a core.
	triesCeiling = 40_000
)

type pairing struct {
	mu      sync.Mutex
	r       *proof.Responder
	code    string
	started time.Time
	msg3    []byte // what was answered, so an identical retry gets the identical bytes
	reply   []byte
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var carrierGrade = netip.MustParsePrefix("100.64.0.0/10")

// sourceKey is what a failed Pair counts against: a public address, or a whole /64 on IPv6. A private
// source, which is what a reverse proxy looks like, counts only in the total.
func sourceKey(source string) string {
	addr, err := netip.ParseAddr(source)
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || carrierGrade.Contains(addr) {
		return ""
	}
	if addr.Is6() {
		p, err := addr.Prefix(64)
		if err != nil {
			return ""
		}
		return p.String()
	}
	return addr.String()
}

// tooManyFailed notes one failed Pair and says whether its answer becomes 429. The limit only ever
// changes that answer: a message that fits a code always goes through, so a stranger cannot lock a
// real code out. Sources stay in memory only. Each list keeps only the times its limit needs, and the
// sources are swept once a window, so a failure costs the same however many came before it: this
// runs under the lock every console's request takes.
func (h *Handler) tooManyFailed(source string) bool {
	key := sourceKey(source)
	h.log.PairFailed()
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	cutoff := now.Add(-triesWindow)
	h.allTries = newest(append(recent(h.allTries, cutoff), now), triesInAll+1)
	if now.Sub(h.swept) >= triesWindow {
		h.swept = now
		for s, list := range h.tries {
			if list = recent(list, cutoff); len(list) == 0 {
				delete(h.tries, s)
			} else {
				h.tries[s] = list
			}
		}
	}
	if _, known := h.tries[key]; key != "" && (known || len(h.tries) < triesSources) {
		h.tries[key] = newest(append(recent(h.tries[key], cutoff), now), triesPerAddress+1)
	}
	return len(h.allTries) > triesInAll || (key != "" && len(h.tries[key]) > triesPerAddress)
}

// newest keeps the last n times of a list, in the list's own memory.
func newest(list []time.Time, n int) []time.Time {
	if len(list) <= n {
		return list
	}
	copy(list, list[len(list)-n:])
	return list[:n]
}

// overOwnLimit says whether a public source has already failed too often. It is refused before any
// work, and does not count toward the ceiling, so one host cannot use the ceiling up for everyone. A
// source with no failures is not added to the table.
func (h *Handler) overOwnLimit(source string) bool {
	key := sourceKey(source)
	if key == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list, ok := h.tries[key]
	if !ok {
		return false
	}
	list = recent(list, h.now().Add(-triesWindow))
	h.tries[key] = list
	return len(list) >= triesPerAddress
}

// underCeiling holds the work of trying messages down, in codes tried, whatever else is happening.
func (h *Handler) underCeiling(codes int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now().Unix()
	if now != h.ceilingSecond {
		h.ceilingSecond, h.ceilingCount = now, 0
	}
	h.ceilingCount += max(codes, 1)
	return h.ceilingCount <= triesCeiling
}

// pairFirst is message 1: the joiner's proof, checked against every waiting code.
func (h *Handler) pairFirst(w http.ResponseWriter, r *http.Request, msg []byte) {
	if len(msg) != pairFirstSize {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if h.overOwnLimit(sourceOf(r)) {
		h.log.PairFailed()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	st := h.store.Snapshot()
	now := h.now()
	var ks [][]byte
	var ids []string
	for _, c := range st.Codes {
		if c.Live(now) {
			ks = append(ks, c.K)
			ids = append(ids, c.ID)
		}
	}
	if !h.underCeiling(len(ks)) {
		h.log.PairFailed()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	responder, err := proof.TryFirst(ks, h.key, msg)
	if err != nil {
		if h.tooManyFailed(sourceOf(r)) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	hello, _ := json.Marshal(proof.Hello{Protocol: proof.Protocol, Oldest: proof.Oldest, Name: st.Settings.Name})
	second, err := responder.SecondMessage(hello)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	idBytes := make([]byte, proof.PairIDSize)
	if _, err := rand.Read(idBytes); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	code := ids[responder.Index]
	h.mu.Lock()
	h.makeRoomLocked(code)
	h.pairings[string(idBytes)] = &pairing{r: responder, code: code, started: now}
	h.mu.Unlock()
	writeBytes(w, append(idBytes, second...))
}

// makeRoomLocked keeps pairings in progress to their time and their number, oldest first. A replay of
// someone's message 1 can push out only that code's own pairings, not anyone else's.
func (h *Handler) makeRoomLocked(code string) {
	now := h.now()
	for id, p := range h.pairings {
		if now.Sub(p.started) > pairingLife {
			delete(h.pairings, id)
		}
	}
	h.dropOldestLocked(pairingsPerCode, func(p *pairing) bool { return p.code == code })
	h.dropOldestLocked(maxPairings, func(*pairing) bool { return true })
}

func (h *Handler) dropOldestLocked(limit int, counted func(*pairing) bool) {
	count := 0
	var oldestID string
	var oldest time.Time
	for id, p := range h.pairings {
		if !counted(p) {
			continue
		}
		count++
		if oldestID == "" || p.started.Before(oldest) {
			oldestID, oldest = id, p.started
		}
	}
	if count >= limit && oldestID != "" {
		delete(h.pairings, oldestID)
	}
}

// consoleJoining is what a console says in message 3.
type consoleJoining struct {
	Kind     string `json:"kind"`
	Protocol int    `json:"protocol"`
	Oldest   int    `json:"oldest"`
	Stream   int    `json:"stream"`
	Account  string `json:"account"`
	Username string `json:"username"`
	Hosts    *bool  `json:"hosts"`
}

// joinsOnly reads `hosts`, which only an app that never hosts sends, as false.
func joinsOnly(hosts *bool) bool {
	return hosts != nil && !*hosts
}

type joinResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Console int    `json:"console,omitempty"`
}

// pairSecond is message 3: the joiner's key and who they are. The code is used up here, under the
// store's lock, so the first to finish takes the place. The answer is kept for the rest of the
// pairing's time: the server cannot know it arrived, and an identical retry gets the same bytes,
// because sealing again would use nonce 0 twice.
func (h *Handler) pairSecond(w http.ResponseWriter, r *http.Request, rest []byte) {
	if len(rest) <= proof.PairIDSize {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, msg3 := string(rest[:proof.PairIDSize]), rest[proof.PairIDSize:]
	h.mu.Lock()
	p := h.pairings[id]
	h.mu.Unlock()
	if p == nil || h.now().Sub(p.started) > pairingLife {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reply != nil {
		if bytes.Equal(p.msg3, msg3) {
			writeBytes(w, p.reply)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	joining, err := p.r.Third(msg3)
	if err != nil {
		// A failed read leaves the handshake unusable, so this pairing is over.
		h.mu.Lock()
		delete(h.pairings, id)
		h.mu.Unlock()
		if h.tooManyFailed(sourceOf(r)) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	json.Unmarshal(joining, &kind)
	var reply any
	switch kind.Kind {
	case "console":
		reply = h.joinConsole(p, joining)
	case "server":
		reply = h.joinServer(p, joining)
	default:
		reply = joinResult{Error: "wrong_code"}
	}
	plain, err := json.Marshal(reply)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sealed, err := p.r.SealReply(plain)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.msg3, p.reply = bytes.Clone(msg3), sealed
	writeBytes(w, sealed)
}

func (h *Handler) joinConsole(p *pairing, raw []byte) joinResult {
	var j consoleJoining
	if json.Unmarshal(raw, &j) != nil {
		return joinResult{Error: "bad_request"}
	}
	st := h.store.Snapshot()
	code := st.Code(p.code)
	if code == nil || code.Kind == store.CodeLink || !code.Live(h.now()) {
		return joinResult{Error: "wrong_code"}
	}
	result, err := h.hub.joinConsole(*code, p.r.Key, j)
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	return result
}

// joinServer is another server using a link code. A key that is a console here, or this server's own,
// is refused with the code kept. Otherwise the code goes as soon as it is used, whatever the link makes
// of it, so it cannot be tried twice.
func (h *Handler) joinServer(p *pairing, raw []byte) any {
	if bytes.Equal(p.r.Key, h.key.Public) {
		return joinResult{Error: "wrong_code"}
	}
	var c store.Code
	err := h.store.Update(func(st *store.State) error {
		code := st.Code(p.code)
		if code == nil || code.Kind != store.CodeLink || !h.now().Before(code.Expires) || st.ConsoleByKey(p.r.Key) != nil {
			return ErrUnavailable
		}
		c = *code
		st.RemoveCode(p.code)
		return nil
	})
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	reply, err := h.links.Accept(c, p.r.Key, raw)
	if err != nil {
		return joinResult{Error: "wrong_code"}
	}
	return reply
}
