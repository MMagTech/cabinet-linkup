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
   pairing. An invite code makes its user the chosen account's friend at once, or, used
   by that very account, puts its console in with them, and a link code pairs at once.
   It works once, and the owner sees the username on the People list and can remove
   them.

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
stops at its expiry, 7 days after it was made. A person's own code is the exception:
it works for each console signed in as that person, as often as needed, until the
owner changes it. An invite, or a console's own code, becomes the code of the person
who uses it, unless they have one already.

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

A code has one kind, set when it is made: a console's own, an invite, a reinstall, a
person's own or a link. The first four are used by a console and the last by a server.
A reinstall code is no longer made: a reinstalled console types its person's own code, and
comes in as one more console of theirs.

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
  fit the code's kind is refused and the code is not used. A person's own code is never
  used up, and is refused for any `account` but theirs. A key the server already
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
minute. Failures are logged as counts only. At most 4096 public sources are counted at once; past that
a new one counts only in the total, as a private source does, so a guesser with many addresses cannot
make each failure cost the server more than the last.

**Offline guessing.** Whoever captures message 1, or a fake server that receives
it, can try guesses at leisure, at one Argon2id per guess, over about 2^57.7 codes.
A code is good for 7 days at most and is used up the moment the real pairing
completes, so a guess only helps someone who blocks the real pairing and finishes
the search inside those 7 days. A person's own code has no expiry, so for it the
search has no such limit: an owner who thinks a code has been seen changes it, which
also signs that person out of every console.

### Who is joining

The payload of message 3 is JSON. A console:

    {"kind": "console", "protocol": 1, "oldest": 1, "stream": 1,
     "account": "<stable id>", "username": "alex"}

`account` is a string the console picks that stays the same for that account on that
console: a hash of the RomM server's identity and the account's id, so that the same small
id on a different RomM server is a different person. The server looks it up only within one
console and the consoles in with it (below), to recognise the account after a reinstall and on
another console, and shows only `username`. Only a reinstall code moves a console to a new key.

An app that only joins and never hosts, as 2P Start, adds `"hosts": false` here and in `signin`, and
missing means it hosts. Two people who both only join can never play, so the owner's page does not
offer them to each other, and an invite made for one does not make the other its friend.

A server, linking:

    {"kind": "server", "protocol": 1, "oldest": 1,
     "name": "Riley's", "address": "linkup.example.org:7845",
     "account": {"id": "<person id>", "username": "maya", "key": "<base64>", "stream": 1}}

The reply:

    console: {"ok": true, "console": 3}
    server:  {"ok": true, "name": "Alex's", "account": {"id": ..., "username": ..., "key": ..., "stream": ...}}
    refused: {"ok": false, "error": "wrong_code" or "bad_request"}

A joiner that gets no answer to message 3 sends the same bytes again.

### One account on more than one console

One `account` is one person on every console it is on: shown once, never in their own friends
list, with one set of friends.

- **A console joins someone** by using their own code, or an invite code made for them, while
  signed in as their own account. From then on it is in with their consoles: an account signing in on it that is already on
  one of them is that same person. A console is in with another only through someone on both, and an
  invite used by any other account brings in only new people, because `account` is not a secret.
  This trusts whoever holds someone's code, or an invite made for them: by sending that person's
  `account`, they join as them.
- **A friend is given one console's key,** and a knock rings that console only. It is the console of
  an invite or session the person is part of, because its boxes are sealed for that key. Otherwise it
  is the one they signed in on, or came back to from Offline, last, of those showing them Online, and
  with none, the one they used last. An invite or session stays on the consoles it began on: only
  those can answer, signal or end it, and one of them going away ends it.
- **Status** is the most of what their consoles say: Linked up on any of them is Linked up, and Online
  on any is Online. A console's own reasons for Offline stay with that console. Offline the person
  chose, sent as `status` with `chosen`, holds on all of them until Online is chosen the same way:
  their other consoles hear a `chosen` event, and `signin` answers it.
- **The owner signs out a person, not a console.** Changing their code takes them off every console
  they are on, and those consoles off the server, until a code is typed on each again; whoever else
  is on one of them comes back with it. Removing the person does the same and drops them. `leave`
  takes a console off its people: someone on no other console goes with it.

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
| `signin` | `account`, `username`, `status`; `hosts`: `false` from an app that only joins | `{"person": "<id>", "chosen": "online" or "offline"}`, `chosen` being the person's own choice. A new account appears on the owner's page, with no friends. The account signed in before this one shows Offline, unless it is on another console. |
| `signout` | | |
| `status` | `status`: `online`, `offline`, `linkedup`; `chosen`: `true` when the person chose Online or Offline, which then holds on all their consoles | |
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
| `leave` | | The console and its accounts are gone from the server, apart from those on another console. |

**A friend,** in `friends`:

    {"id": "<id>", "username": "alex", "server": "Riley's", "status": "online",
     "key": "<base64>", "stream": 1, "invite": {"id": "...", "sealed": "..."}}

`invite` is there only while they have invited you, on the console the knock rings. `key` is the
key of one of the friend's consoles, and can change with a `friends` event. `status` is `online`, `linkedup` or
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
| `chosen` | `status` | The person chose Online or Offline on another of their consoles, and it holds on this one too. |

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
for the session, not its id. A connection has started once the relay has given it a port: one
turned away for the session's 4 has not, and cannot open after the 10 minutes.

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

## Between two consoles: the game

Once a friend has joined, the two consoles connect to each other by WebRTC, and the game goes straight
between them, or through a relay. The server only carries the connection details, sealed. The host runs
the game and sends its picture and sound. The friend is player two: it shows the picture, plays the
sound, and sends its controller. An app that only joins (`"hosts": false`) is always the friend, and
needs only the friend's half of this. This is stream version 1.

### Getting connected

Each piece of connection details goes in a signal box of its own (`signal`, above), which CabinetOS
seals to expire 5 minutes later. The box's `details` is JSON text, `{"g": <number>, "d": <text>}`. `g`
is the connection's number, 1 to begin with (see "A drop"), and `d` is JSON text again, one of these:

| `d` | From | |
|---|---|---|
| `{"hello": {"stream": 1, "oldest": 1, "h264": true, "h265": true}}` | The friend | Its first word, below |
| `{"kind": "offer", "sdp": "..."}` | The host | |
| `{"kind": "answer", "sdp": "..."}` | The friend | |
| `{"ice": "candidate:...", "m": 0}` | Both | One ICE candidate, as it is found; `m` is the index of its media line |

In order:

1. **The join.** The friend's `join` answers with the session, and the host hears `joined`. Each then
   asks its own server for `config` and for `relay`. Each takes the first STUN server in `config`'s list
   that answers, and the first of the relay's `urls`, a `turn:` address it uses over UDP, with its
   username and password. The server picks the relay: the host's server's first (see "The relay").
2. **The hello.** The friend sends it straight after the join, before anything else. `h264` and `h265`
   say which it can decode. `stream` is its stream version and `oldest` the oldest it still speaks, and
   either missing reads as 1.
3. **The host checks it.** A friend whose `stream` is below the host's is behind, and a host whose
   `stream` is below the friend's `oldest` is behind. Either way the two do not connect: the host names
   who is behind, sends `report` with `failed` and `behind`, and ends the session, and the friend hears
   only `session_ended`. The host picks H.265 when both consoles can use it, and H.264 when only that;
   with neither, they cannot play.
4. **The offer.** Once it has the hello, `config` and `relay`, the host makes its connection, with the
   picture, the sound and both data channels in it, and sends the offer. The friend makes its own once
   it has `config` and `relay`, and answers. The host always offers, and the friend never does.
5. **Candidates** go both ways as each side finds them. A piece that arrives before its side has made
   its connection waits, and is used in order once it has. A console gathers UDP candidates only, on
   its ports 47710 to 47714, so a friend's app needs UDP.
6. **Connected.** Each side sends `report` with `direct`, or `relay` when its own end goes through a
   relay, and again for each new connection (see "A drop"). With no connection 30 seconds after the
   join, each gives up and sends `report` with `failed`.

The connection is one bundle. The host's picture and sound are send-only, and the friend sends nothing
on them but its RTCP reports, which both sides send about once a second. The friend asks for a lost
packet again (NACK), the host resends it, and the friend waits up to 100 ms for the resend.

### The data channels

The host makes both, before its offer. The friend makes none, and takes them as they open.

| Channel | Made | Carries |
|---|---|---|
| `fast` | Unordered, never resent (`maxRetransmits` 0) | Bytes: what a later message replaces, where a resend would only be late |
| `control` | Ordered and reliable | Text: what must arrive |

On `fast`, the first byte says what a message is. A reader ignores a first byte it does not know, and
reads a message longer than it expects as far as it knows.

| Byte | Name | From | Bytes | |
|---|---|---|---|---|
| `P` (0x50) | Pad | The friend | 44, or 20 from an older console | The controller, below |
| `R` (0x52) | Rumble | The host | 8 | Below |
| `B` (0x42) | Beat | Both | 9 | Every 250 ms. Then the sender's own clock in milliseconds, a signed 64-bit number |
| `b` (0x62) | Beat back | Both | 9 | The beat it answers, its first byte changed to `b`, sent at once |
| `K` (0x4B) | Full picture | The friend | 1 | Asks the host for a fresh full picture (see "The picture") |

A beat's clock means something only to its sender, which takes the difference as the round trip, for
its log.

On `control`, every message is text:

| Text | From | |
|---|---|---|
| `picture:1` | The host | The game's picture and sound are going out |
| `picture:0` | The host | Something else is on the host's screen, and no picture and no sound go out |
| `bye` | Both | Goodbye (see "Goodbye") |
| `p:` and JSON | Both | A word for the other console's screens (see "Words between the screens") |

### The controller

From the friend, each time its state changes, and every tenth of a second when it does not. It starts as
soon as the two are connected: the host gives player two the motion sensors that `senses` names, and
before the first game waits up to a second to hear a state. While the friend's own screens are over the
picture, it sends its controller let go: no button, sticks centred, no turning, and held at its last
tilt.

Packed, with nothing between fields, and every number little-endian:

| Offset | Bytes | Field | |
|---|---|---|---|
| 0 | 1 | `tag` | `P` |
| 1 | 1 | `senses` | The motion sensors the controller has: 1 an accelerometer, 2 a gyroscope, added together |
| 2 | 2 | `seq` | Unsigned, one more with each message |
| 4 | 4 | `buttons` | A bit for each button held, numbered below |
| 8 | 12 | `axes` | Six signed 16-bit numbers, in the order below |
| 20 | 12 | `accel` | Three 32-bit floats, x, y, z: metres per second squared, gravity included |
| 32 | 12 | `gyro` | Three 32-bit floats, x, y, z: radians per second |

**Only the highest `seq` counts,** since messages on `fast` can be lost and overtake each other. The
host takes a state whose `seq`, less the last it took, is above 0 as a signed 16-bit number, so the count
can wrap, or is 1000 or more below it, which is a count started again. It drops the rest.

**The buttons and axes are numbered as SDL 3 numbers them,** listed here so nobody needs SDL. Bit 5,
Guide, is the console's own button, and a friend never sets it.

| Bit | Button | Bit | Button |
|---|---|---|---|
| 0 | South: the bottom face button, A on an Xbox pad | 13 | D-pad left |
| 1 | East: the right face button, B | 14 | D-pad right |
| 2 | West: the left face button, X | 15 | Misc 1: Share, Capture or a microphone button |
| 3 | North: the top face button, Y | 16 | Right paddle 1, the upper |
| 4 | Back | 17 | Left paddle 1, the upper |
| 5 | Guide | 18 | Right paddle 2, the lower |
| 6 | Start | 19 | Left paddle 2, the lower |
| 7 | Left stick, pressed | 20 | Touchpad, pressed |
| 8 | Right stick, pressed | 21 | Misc 2 |
| 9 | Left shoulder | 22 | Misc 3 |
| 10 | Right shoulder | 23 | Misc 4 |
| 11 | D-pad up | 24 | Misc 5 |
| 12 | D-pad down | 25 | Misc 6 |

| Axis | | Range |
|---|---|---|
| 0 | Left stick, across | -32768 left to 32767 right |
| 1 | Left stick, up and down | -32768 up to 32767 down |
| 2 | Right stick, across | As the left |
| 3 | Right stick, up and down | As the left |
| 4 | Left trigger | 0 let go to 32767 pressed all the way |
| 5 | Right trigger | As the left |

**The tilt** is SDL's too. For a controller held in front of you, x runs from left to right, y from
bottom to top, and z from away from you to towards you, so a controller lying flat reads about +9.81 on
y. A turn is positive when it looks anticlockwise from the positive end of its axis. A sensor that
`senses` does not name reads 0.

**An older console** sends 20 bytes, as far as the end of `axes`, and its second byte is spare: the host
reads it as a controller with no motion sensors. An older host reads a newer friend's first 20 bytes and
ignores the rest. Anything under 20 bytes is not a pad.

### Rumble

From the host, each time a game rumbles player two. Packed and little-endian:

| Offset | Bytes | Field | |
|---|---|---|---|
| 0 | 1 | `tag` | `R` |
| 1 | 1 | `reserved` | 0 |
| 2 | 2 | `low` | The low-frequency motor, the left: 0 to 65535 |
| 4 | 2 | `high` | The high-frequency motor, the right: 0 to 65535 |
| 6 | 2 | `ms` | How long, which is always 300 |

A CabinetOS friend rumbles for 300 ms, whatever `ms` says. Each rumble replaces the one before, as SDL's
own does, so 0 and 0 stops it. It travels on `fast`, so one can be lost.

### The picture

H.265 or H.264 (step 3 above), payload type 96, in RTP packets of at most 1200 bytes.

- **Its size** is the host's own screen's, at most 1080 lines high: a 4K host sends 1920 by 1080. A
  host whose connection cannot keep up drops from 1080 lines to 720, and climbs back when it can, so
  the size can change in the middle of a game. Each new size starts with a full picture.
- **Its colours are BT.601, limited range.** A decoder guesses BT.709 at these sizes, so the friend
  states BT.601 itself.
- **Each picture can be shown the moment it is decoded:** there are no B-frames and one reference
  frame. CabinetOS's friend shows each one without waiting on its timestamp, and of those waiting keeps
  only the newest.
- **A full picture comes** at least every 600 frames, at each bitrate step, whenever the host's picture
  comes back on, and whenever the friend asks, with the parameter sets in front of each one.
- **The friend asks for one** with `K`. A picture that loses a whole frame's packets freezes the stream
  until the next full picture, and that one can lose a packet too. CabinetOS's friend asks once by
  RTCP, the usual WebRTC way, and that is not enough. So while packets are arriving and no picture has
  come out of the decoder for 150 ms, the friend sends `K`, and again every 150 ms until one does.
- **The bitrate is steady, at most 18 Mbit/s,** and the host steps it by the loss the friend's RTCP
  reports show: 18, 12 and 8 Mbit/s at the host's own size, then 6 and 3.5 Mbit/s, at 720 lines when
  the host's own is 1080. More than 3% lost for 2 seconds in a row is a step down, and under 0.5% for 20
  seconds a step up. Losing more than 3% for 6 seconds at the lowest step tells both consoles the
  connection is too slow (`slow`, below), at most every 10 minutes.

The host starts with its picture off: before the first `picture:1` there is black at most.
`picture:0` means something other than the game is on the host's screen, such as its menus: no picture
and no sound go out until `picture:1`.

### The sound

Opus at 48,000 Hz, stereo, payload type 97, in 10 ms pieces at a steady 128 kbit/s. It stops and starts
with the picture.

The host's sound clock is not the friend's: one host was measured sending 47,960.6 samples a second by
the friend's clock, and a friend playing at its own pace ran dry after 30 to 70 seconds. So CabinetOS's
friend plays the sound through a queue of its own and trims its pace to keep the queue steady, by at
most 2000 parts per million either way. It keeps 40 ms in hand to begin with, 20 ms more after each
stall it did not cover, up to 100 ms, and 10 ms less after each two calm minutes. It waits up to 60 ms
for a missing piece, then lets Opus fill the gap. A queue dry for more than 300 ms is the host gone
quiet, not the network.

### Words between the screens

`p:` then a JSON object whose `t` is the word. A reader ignores a word, or a field, it does not know.

| `t` | From | Fields | |
|---|---|---|---|
| `starting` | The host | `game` | A game is starting for both, by its name, which need not be the invite's |
| `place` | The host | `x`, `y`, `w`, `h` | Where the game sits on the host's screen, in points of a 1920 by 1080 canvas, so the friend can draw the bars around it as the host's TV does. Said when a game CabinetOS draws itself first shows, and whenever it moves; an emulator in a window of its own says nothing |
| `paused`, `resumed` | The host | | The host's own pause menu opened, or closed |
| `idle` | The host | | Neither player has pressed anything for a while, and both pause |
| `quit` | The host | | The game ended. The session goes on, and the friend waits for the next |
| `ended` | The host | | The host ended the session |
| `slow` | The host | | The connection cannot keep up, even at the lowest step |
| `pause` | The friend | | The friend paused, and the host's game stops too |
| `leave` | The friend | | The friend left. The host's game carries on without player two |
| `resume` | Both | | Carry on, after a pause or `idle` |
| `press` | Both | | Somebody pressed something on this side, said at most every 2 seconds, so neither console counts the two as idle |

### A drop

Anything heard on either channel counts, and the beat is there so that a quiet game still says
something.

- **A drop** is 2 seconds with nothing heard. Under that, nothing shows: the friend's last state holds,
  and lost packets are resent.
- **Back** is anything heard again. The host then sends a full picture, and `picture:1` or `picture:0`
  again. On a new connection that word can go before `control` is open, and be lost, so a friend keeps
  the last `picture:` it heard.
- **A new connection** is only ever the host's to make. After 8 seconds with nothing heard, and then at
  most every 15 seconds while it lasts, the host makes a fresh connection for the same game, raises `g`
  by one, and sends a new offer and new candidates under it. A friend that gets a piece with a higher
  `g` than its own drops its connection, makes a new one, and answers under that `g`. Either side drops
  a piece under a lower `g` than its own. The hello is not sent again, and the codec stays the same.
- **Giving up** is the friend's. After a minute of drop, CabinetOS's friend sends `end`, and the
  host hears `session_ended`.

### Goodbye

- **A person ends it.** The friend says `leave`, or the host says `ended`, and then sends `end` to its
  server, whose `session_ended` reaches the other console.
- **`bye` is from a console going away:** its LinkUp program stopping, or its main program going while
  a game is on. 2P Start says it when it quits. Whoever hears it takes the session as over, and on the
  host the game carries on without player two.

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
