# The LinkUp protocol

The one definition of what consoles and servers say to each other. The server
in this repository and the console's `cabinetos-linkup` program share no code;
they share this file. Protocol version 1, stream version 1.

## What it has to do

Everyone who runs a server is at home with a forwarded port. Most have no
certificate, some put a reverse proxy in front, and the address can be a name, a
plain IP, IPv6 or a VPN address. So the protocol holds to three requirements:

1. **It works on a plain IP**, with no certificate.
2. **It works behind a reverse proxy**, which may read and rewrite anything it
   carries, and which holds no key.
3. **A fake server cannot pass.** Someone sitting between a console and its
   server, or posing as the server at its address, cannot pose as either side,
   unless they hold the code. A code travels in an ordinary message, so whoever
   holds it can use it first and take that place, or sit in the middle of that one
   pairing. An invite code makes its user the chosen account's friend at once, and a
   link code pairs at once. It works once, and the owner sees the username on the
   People list and can remove them.

And two more that follow from those:

4. **The code never travels.** Only a proof made from a slow hash of it does.
5. **Everything is encrypted, every message.** HTTP here is only a way of moving
   sealed bytes. Whoever watches the wire sees sizes, times, addresses, what kind
   of message each is, its session id, its counter and its status, and nothing
   else: no code, username, friend, game or connection detail.

**What this does not protect against.** A server that has been broken into can
read what passes through it apart from what consoles seal for each other, and can
hand out a false key or false connection details. So can a linked owner, or a break-in
at a linked server, for the people it hosts. That is accepted (see "What a hacked server
gets"). A server never holds a RomM login.

## Version numbers

Two numbers, each stated on connect:

- `protocol`: console to server, and server to server.
- `stream`: console to console.

Changes only add: new fields and new operations, never a rename or a removal. A
reader ignores what it does not know. A number moves only on a real break, and
each side also states `oldest`, the oldest `protocol` it still speaks. A side
whose number is below the other's `oldest` is behind, and the console names who:
it is never forced to update.

## Moving bytes

Everything is one HTTP request: `POST <base>/linkup/v1`, body and answer
`application/octet-stream`.

`<base>` comes from the address the owner typed, which is free text:

| Typed | Tried |
|---|---|
| `http://...` or `https://...` | As typed |
| A host and a port | `http://host:port` |
| A host alone | `https://host`, then `http://host` |

An IPv6 address is in brackets. A path after the host is kept, for a proxy. A
certificate is not what makes this safe, so a reader need not check one, and a
self-signed proxy works. TLS only hides each message's kind, session id and counter
from a watcher, and stops them capturing a first message.

A reader never follows a redirect, and takes `http` and `https` only.

The body's first byte says what it is:

| Byte | Name | Then | The answer |
|---|---|---|---|
| 1 | Pair, first | Noise message 1 (48 bytes) | `pair_id` (16 bytes), Noise message 2 |
| 2 | Pair, second | `pair_id` (16 bytes), Noise message 3 | Sealed reply |
| 3 | Open | Noise message 1 | Noise message 2 |
| 4 | Sealed | `session` (8 bytes), `counter` (8 bytes, big endian), ciphertext | Ciphertext |

The answers that are not 200, all with an empty body:

| Status | Meaning |
|---|---|
| 403 | Pair: no waiting code matches. The console says "Wrong or expired code". |
| 429 | Pair: this public source has failed too often, or the server is at its ceiling, or no waiting code matches and too many have failed lately. The console says "Too many tries". |
| 404 | Everything else, including every other path, method and malformed body, an Open from a key this server does not know, a replayed Open, and an unknown or closed session. |

None of these is authenticated, so none of them can do more than make a console
say it failed. A stranger can tell this is a LinkUp server from a 403, and little more.

A request is at most 64 KiB: anything larger is 404. An answer is at most 256 KiB: a server keeps
its own answers under that, and a reader stops reading past it.

## The code

Twelve characters in three groups of four, `K7MR-4QXT-9HPV`, drawn at random from
`ACDEFGHJKMNPQRTUVWXYZ2345679` (28 characters, about 57.7 bits). A reader accepts
what `23456789ABCDEFGHJKMNPQRSTUVWXYZ` allows, any case, and any dashes or
spaces; it upper-cases and removes the rest before hashing. A code works once and
stops at its expiry, 7 days after it was made.

The slow hash, `K`:

    K = Argon2id(version 0x13,
                 password = the 12 characters,
                 salt     = "cabinet-linkup 1", exactly 16 bytes,
                 time = 3, memory = 65536 KiB, lanes = 1, length = 32 bytes)

For `K7MR4QXT9HPV`, `K` is
`c3c7e78d316b58eff025cd98473fc042a0ebd4ff209be27d491583c495e8b6ad`.

The parameters are fixed by the protocol and are never negotiated, so a fake
server cannot ask for a weaker hash. The salt is fixed because a console knows
only an address and a code before its first message, and the address it typed may
not be the one another console typed. The server works out `K` once, when the code
is made, and keeps it beside the code. Checking a guess at the server therefore
costs the guesser the slow hash and the server almost nothing.

A code has one kind, set when it is made: a console's own, an invite, a reinstall or a
link. The first three are used by a console and the last by a server.

## Pairing

`Noise_XXpsk0_25519_ChaChaPoly_SHA256` ([the Noise spec](https://noiseprotocol.org/noise.html)),
with `K` as the pre-shared key and the prologue `cabinet-linkup pair 1`. The side
that has the code and is joining is the initiator. Each side has one long-term
X25519 key: a console makes its own the first time LinkUp is turned on, a server
the first time it starts.

The Noise spec wants a 256-bit pre-shared key, and `K` carries 57.7 bits of the code. This
departs from it knowingly, and the only consequence is the offline guessing priced below. A
real PAKE would turn that price from tens of billions of dollars into impossible, at the
cost of a second protocol beside Noise, and of a public id for each code. These codes are
random, used once and short-lived, so nothing here spends that margin.

    -> psk, e                 Pair, first. Empty payload.
    <- e, ee, s, es           Answer. Payload: {"protocol":1,"oldest":1,"name":"Riley's"}
    -> s, se                  Pair, second. Payload: who is joining.
    <- (transport message)    Sealed reply: the result.

- **Message 1 is the joiner's proof.** It ends in an authentication tag under a
  key that depends on `K`. It is exactly 48 bytes: the server refuses anything else
  before trying it. The server tries it against each waiting code's `K` (cheap, since
  `K` is already worked out) and answers 403 if none fits. A stranger without a code
  gets no key and no salt. Message 1 can be replayed while its code lasts, which is why
  a replay can take a slot only from its own code.
- **Message 2 is the server's proof.** Its static key and payload are encrypted
  under a key that depends on `K` and on both ephemeral keys, so a fake server
  cannot make one, and a console that cannot open it stops and keeps nothing.
- **Message 3 binds the joiner's key.** The server checks it, then uses up the code
  under a lock, so the first to finish takes the place. A joiner whose `kind` does not
  fit the code's kind is refused and the code is not used. A key the server already
  knows is refused unless the code is a reinstall code for that console, or the key is
  a linked server adding a pair, or a console that was removed.
- **Both long-term keys are hashed into the handshake,** so nobody can swap either
  without `K`. A joiner remembers the server's key only after the sealed reply. The
  server saved the joiner's when it answered, and it cannot know the reply arrived, so it
  keeps the reply's exact bytes for the rest of that pairing's 60 seconds and answers an
  identical message 3 with them.

The sealed reply is under the second key from Noise's Split, with nonce 0 and no
associated data.

A pairing in progress lives 60 seconds in memory, at most 32 at a time and 4 per code,
oldest dropped first. At most 200 codes wait at once.

**Rate limit.** Only a failure counts: 5 a minute per public address (per /64 on IPv6), and 30 a
minute in all. A public source past its own 5 is refused before anything is tried, and does not count
toward the ceiling. Past the total, a message that fits a code still goes through and only a failure's
answer changes from 403 to 429, so a stranger cannot lock a real code out. A private source, which is
what a reverse proxy looks like, counts only in the total. A ceiling of 40,000 codes tried a second
holds the work down, about a tenth of a core, so a server with 2 waiting codes needs 20,000 requests a
second before anyone is refused. Someone sharing a public address with a guesser is refused too, for a
minute. Failures are logged as counts only.

**Offline guessing.** Whoever captures message 1, or a fake server that receives
it, can try guesses at leisure, at one Argon2id per guess, over about 2^57.7 codes.
A code is good for 7 days at most and is used up the moment the real pairing
completes, so a guess only helps someone who blocks the real pairing and finishes
the search inside those 7 days.

### Who is joining

The payload of message 3 is JSON. A console:

    {"kind": "console", "protocol": 1, "oldest": 1, "stream": 1,
     "account": "<stable id>", "username": "alex"}

`account` is a string the console picks that stays the same for that account on that
console: a hash of the RomM server's identity and the account's id, so that the same small
id on a different RomM server is a different person. The server looks it up only within one
console, to recognise the account after a reinstall, and shows only `username`. Only a
reinstall code moves a console to a new key.

A server, linking:

    {"kind": "server", "protocol": 1, "oldest": 1,
     "name": "Riley's", "address": "linkup.example.org:7845",
     "account": {"id": "<person id>", "username": "maya", "key": "<base64>", "stream": 1}}

The reply:

    console: {"ok": true, "console": 3}
    server:  {"ok": true, "name": "Alex's", "account": {"id": ..., "username": ..., "key": ..., "stream": ...}}
    refused: {"ok": false, "error": "wrong_code" or "bad_request"}

A joiner that gets no answer to message 3 sends the same bytes again.

## Sessions

Once paired, every connection opens a session. The address can change, because the
key is what is checked, so a retyped address needs no new pairing.

`Noise_IK_25519_ChaChaPoly_SHA256`, prologue `cabinet-linkup session 1`. The
initiator already holds the responder's remembered key.

    -> e, es, s, ss           Open. Payload: {"protocol":1,"oldest":1,"stream":1,"t":<t>}
    <- e, ee, se              Answer. Payload: {"session":"<16 hex>","protocol":1,"oldest":1}

`t` is an integer: the larger of the time in nanoseconds since 1970 and one more than the last `t` this
side sent. A console keeps its last `t` on disk, so a clock that moves back cannot lock it out; a server
opening toward a linked server keeps it in memory. The server keeps the highest `t` per known key in
memory and answers 404 to anything not above it, with no state kept. So one side makes one Open at a time.

- **An unknown key gets 404,** the same as a malformed body. The console port holds
  nothing for a stranger.
- **A key that was removed gets a real answer:** the payload is `{"removed": true}`
  and no session opens. Because it is inside the handshake it is authenticated, so
  a fake server cannot make a console say "Removed from the LinkUp server".
- **A recorded Open is dropped,** by `t`, so it cannot keep replacing a session as a console
  opens it. After a server restart, with the highest `t` gone, each old recording works at
  most once more: its payload holds only version numbers, and the replayer cannot read the
  answer. A new session replaces the old only once its first Sealed request arrives. The
  server keeps at most one unconfirmed session per key.
- **A key has one kind, set at pairing.** An `op` of the other kind answers
  `{"error":"unknown"}`.

A Sealed request:

    session (8) | counter (8, big endian) | ChaCha20-Poly1305(key = sender's transport key,
                                                              nonce = counter,
                                                              ad = 4 | session | counter)

The first output of Noise's Split is the initiator's send key. The nonce is Noise's own: four
zero bytes and then the counter, little endian. On the wire the counter is big endian.

Counters start at 1 and only go up, and 0 and 2^64-1 are refused. Requests can overtake each
other (a long request and a short one), so the server keeps a window of the last 64 and
refuses a repeat or anything older, with 404. The answer uses the other transport key, the
same counter as its nonce, and `ad = 5 | session | counter`, so an answer cannot be moved to a
different request.

Each nonce is used once under each key because of three rules:

- **A server checks in this order:** known session, then under the session's lock the
  window, the authentication tag, and marking the counter. It marks the counter before it
  does the work, so two copies sent at once get one answer. A request failing any step changes
  nothing, not the window and not the idle timer.
- **A sender uses up a counter when it seals.** A retry is the same bytes or a new counter.
- **Sessions are never saved to disk.**

A session closes after 120 seconds with no request. A session makes at most 50 requests a second,
and past that the answer is `{"error":"busy"}`. Any answer that is not 200 means: open a new session, with
a growing wait between tries, up to 60 seconds. A restart of the server closes every session.

## Between two consoles: sealed boxes

Friends' keys come from the server. What the server only carries (the invite's game
name, and the connection details) is sealed for the other console, in the one-way
pattern `Noise_K_25519_ChaChaPoly_SHA256`, with the prologue
`cabinet-linkup box 1 ` followed by the invite's id:

    -> e, es, ss              Payload: JSON, below.

The inviter picks the invite's id, 16 random bytes as hex, before it seals anything, and sends
it in `invite`, so the first box is bound to it too. While an invite with that id lives, the server
refuses it again, unless it is the same inviter with the same box, so a retry after a lost answer is safe.

The server sees base64 and passes it on. Noise_K has no replay protection, so a box says:

    {"type": "invite" or "signal", "n": <counts up per sender and invite>, "expires": <seconds
     since 1970>, "from": "<sender's username>", "to": "<recipient's username>", ...}

An invite box also holds `stream` and the game's name, and a signal box the connection details. A
console drops a box whose prologue or sender does not fit, whose `n` it has seen, whose `expires`
has passed, or whose `type` is not what it expected. It caps a game's name on receipt and strips
control and direction-changing characters, because the server cannot filter what it cannot read.

**The limit.** A console learns a friend's key from its server. A server that has been
broken into could hand out its own key and read the boxes; the two consoles do not
check each other's keys another way. A later theft of a console's key opens recorded boxes
addressed to it.

## Operations

Inside a session, a request is JSON `{"op": "...", ...}` and an answer is JSON with
`{"error": "<word>"}` when it failed. The server says what happened, never what to
display.

A console has one current account at a time. Every operation is that account's.

| `op` | Fields | Answer |
|---|---|---|
| `signin` | `account`, `username`, `status` | `{"person": "<id>"}`. A new account appears on the owner's page, with no friends. The account signed in before this one shows Offline. |
| `signout` | | |
| `status` | `status`: `online`, `offline`, `linkedup` | |
| `friends` | | `{"friends": [...]}` |
| `config` | | `{"name": "<server>", "stun": ["host:port", ...]}` |
| `wait` | `after` | `{"epoch": "<id>", "events": [...], "next": n}` |
| `invite` | `invite`, `friend`, `sealed` | `{"invite": "<id>", "expires_in": 1800}`, or `unavailable` |
| `cancel` | `invite` | |
| `decline` | `invite` | |
| `join` | `invite` | `{"session": "<id>"}`, or `unavailable` |
| `signal` | `session`, `sealed` | |
| `end` | `session` | |
| `report` | `session`, `path`: `direct`, `relay`, `failed`; `reason`: a word from the list below | |
| `relay` | `session` | `{"relay": {"urls": [...], "username": "...", "password": "..."}}`, or `{"relay": null, "reason": "off" or "limit"}` |
| `leave` | | The console and its accounts are gone from the server. |

**A friend,** in `friends`:

    {"id": "<id>", "username": "alex", "server": "Riley's", "status": "online",
     "key": "<base64>", "stream": 1, "invite": {"id": "...", "sealed": "..."}}

`invite` is there only while they have invited you. `status` is `online`, `linkedup` or
`offline`, and a friend on a linked server that is behind or out of reach is `offline`.
The owner's notes never reach a console.

A box in `invite` is at most 2 KiB, and in `signal` at most 16 KiB. A username is at most 128
bytes, with no control or format characters.

**`wait`** is the one long request, held up to 25 seconds, and it is how a console counts
as on: a console with no request for 75 seconds shows Offline. `after` is the last `next`
it was given, and `epoch` changes when the server restarts, which means fetch `friends`
again and start over at 0. One answer holds at most 64 KiB of events; `next` says where to
carry on.

Any operation from a console taken off the server answers `{"error":"removed"}`, except `wait`,
which still delivers what is queued.

**Events:**

| `type` | Fields | |
|---|---|---|
| `friends` | | Something on the list changed; fetch it again. It does not say what, and it is said once until heard. There is no event for someone coming online. |
| `invite` | `invite`, `from`, `sealed` | |
| `invite_ended` | `invite`, `reason`: `cancelled`, `timeout`, `declined`, `unavailable` | The one event for an invite ending, for either side. |
| `joined` | `session` | To the inviter. |
| `signal` | `session`, `sealed` | |
| `session_ended` | `session` | |
| `removed` | | This console was taken off the server. |

**What the server owns:** each account's status; who can be invited; refusing a knock on
anyone Offline or Linked up; the 30 minutes; declines; a join ending any other invite to
that person; whichever of a join and a cancel reached it first. An inviter counts as
Linked up from the moment of the invite until it ends with nobody joining, or until the
session ends. The server that receives a knock refuses more than 10 in 10 minutes from one
person to another, and ends a knock at 30 minutes by its own clock.

**A session that outlives its people.** A session ends on the server when a console in it has been silent
for 5 minutes, at 24 hours regardless, and when the owner removes the console, the friendship, the pair or
the link. The server tells nobody: the game does not go through it and carries on, but its relay stops and
both people can be knocked on again. A console asked about a session the server has ended answers
`unavailable`, and a console that wants to say it is in a game after that sends `status` `linkedup`.

**`report` reasons,** the only words that reach the log: `none`, `no_path`, `timeout`,
`relay_limit`, `behind`, `other`.

## Between servers

Server to server is the same protocol on the same port: a server pairs with a link code as
a joiner (`kind: "server"`) and opens sessions with the same `Open`. Each side remembers
the other's key and address, and each sends to the other by its own short requests. There
is no long request between servers.

Only people named in a pair are ever mentioned across a link. A server refuses any message
about anyone else, and nothing is passed on to a third server. A pair comes only from a code:
`link_hello` updates what is known of a pair and never adds one. A link operation naming an invite or
session that link is not part of does nothing: it answers `unavailable` to a question, `off` to `link_relay`,
and is ignored otherwise. A `reason` that is not one of the four words reads as `unavailable`.

| `op` | Fields | |
|---|---|---|
| `link_hello` | `name`, `address`, `pairs`: `[{"ours", "theirs", "status", "username", "key", "stream"}]` | Sent every 60 seconds and when something changes. A pair it stops mentioning is gone, unless it is under 5 minutes old, since the two servers may not hear of a new one at the same moment. |
| `link_pair_remove` | `theirs`, `ours` | A pair was removed on the other side. |
| `link_unlink` | | The whole link was removed on the other side. |
| `link_invite` | `invite`, `from`, `to`, `sealed` | Answer: ok or `unavailable`. |
| `link_invite_ended` | `invite`, `reason` | |
| `link_join` | `invite` | Answer: ok or `unavailable`. The inviter's server decides, and the same join again is ok. |
| `link_signal` | `session`, `sealed` | |
| `link_end` | `session` | |
| `link_relay` | `session` | The same answer as `relay`. |

**An address a server is told is only a candidate.** A linked server's address arrives this way:
when it uses a code, and in `link_hello`. So it could name something on your home network, to make
your server send requests there. So:

- A told address is a host, a port and an optional path, with `http` or `https` if it has a scheme.
  Anything holding `?`, `#`, `@`, `%`, a space or a control character is refused. Every request's path
  ends in `/linkup/v1`, and redirects are not followed.
- A candidate is tried with an Open under the remembered key, at most once per 10 minutes per link
  however many are sent, apart from everything else so that nothing shows how it went, and saved only if
  that succeeds. Something on a home network cannot finish that handshake, so it is never adopted. A
  second real change of address within 10 minutes waits its turn.
- Once saved, a told address may not lead to a private address, as the relay's list has them, unless
  the connection that proved it was to one, so a name cannot change to point inside later. An address
  the owner typed is free, so VPN and home-network links keep working.

## The relay

A TURN server in the container, off until its owner turns it on, on UDP (3478 and a small
range, set by the owner). The host's server's relay is used first, and the friend's if the
host's is off.

The range is the ports right after the relay's own, two for each relayed game at once the owner
allows, at most 16 games: 3479 to 3482 for two. A connection gets one of them, so a game uses one or
two, and a connection past the owner's number is refused. The address in `urls` is where the STUN
list sees the server's packets come from, never what its public address's name leads to, which is a
reverse proxy's address behind one.

A `relay` password belongs to one session. A new connection can use it for 10 minutes after
it is given out. A connection that has already started keeps working until the session
ends, at most 24 hours. It is refused as soon as the server has seen the session end, and the
session's connections end then too, so the ports the consoles used are free for their next game. A
session can have 4 connections at once, however many passwords it asks for. The username holds a tag
for the session, not its id.

It refuses to relay to private addresses: IPv4 and IPv6 loopback, private, link-local,
unique-local, carrier-grade (100.64.0.0/10), multicast and unspecified addresses, IPv4 addresses
written inside IPv6 ones, and the ranges kept for documentation and tests. It refuses ports under
1024. It sends to the host's own public address only on the relay's own ports, because two consoles
can share one relay, and that address would otherwise lead to every port the owner forwards. A
connection can have 8 peer addresses.

Each connection is capped at 26 Mbit/s, about what the console's stream reaches at its
short peaks. Bytes are counted through the relay, saved now and then, and when the month's
limit is reached the relay refuses everything until its reset day. Game packets stay
encrypted between the two consoles, by WebRTC, so the relay cannot read them.

## What a hacked server gets

Usernames, who is friends with whom, the owner's notes, console and server public keys, the
addresses of consoles connected at that moment, use of the relay's data, and the chance to
send false connection details or false keys to consoles, including to the friends of its people
on a linked server. A linked owner has this power too, over the people it hosts: they can rename
them or change their key, and can always have invented them. It never holds a RomM login, a game,
a save or play history, and it cannot read a box that was sealed before it was broken into.
The console's LinkUp program is walled off from the home folder that holds the RomM token and
the PIN. A stolen server key stays good until replaced, so a server that was broken into starts
over with a new data folder, and everyone pairs again.
