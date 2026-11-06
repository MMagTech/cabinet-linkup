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
   server, or posing as the server at its address, cannot pose as either side.

And two more that follow from those:

4. **The code never travels.** Only a proof made from a slow hash of it does.
5. **Everything is encrypted, every message.** HTTP here is only a way of moving
   sealed bytes. Whoever watches the wire sees sizes, times and addresses, and
   nothing else: no code, username, friend, game or connection detail.

**What this does not protect against.** A server that has been broken into can
read what passes through it apart from what consoles seal for each other, and can
hand out a false key or false connection details. That is accepted (see "What a
hacked server gets"). It never holds a RomM login.

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
certificate adds nothing to the protocol's safety, so a reader need not check
one: a self-signed proxy works.

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
| 403 | Pair, first: no waiting code matches. The console says "Wrong or expired code". |
| 429 | Pair: too many tries. The console says "Too many tries". |
| 404 | Everything else, including every other path, method and malformed body, an Open from a key this server does not know, and an unknown or closed session. |

None of these is authenticated, so none of them can do more than make a console
say it failed. A stranger learns only that something answers.

A body is at most 64 KiB. Anything larger is 404.

## The code

Twelve characters in three groups of four, `K7MR-4QXT-9HPV`, drawn at random from
`ACDEFGHJKMNPQRTUVWXYZ2345679` (28 characters, about 57.7 bits). A reader accepts
what `23456789ABCDEFGHJKMNPQRSTUVWXYZ` allows, any case, and any dashes or
spaces; it upper-cases and removes the rest before hashing. A code works once and
stops at its expiry, 7 days after it was made.

The slow hash, `K`:

    K = Argon2id(password = the 12 characters,
                 salt     = "cabinet-linkup code 1",
                 time = 3, memory = 64 MiB, lanes = 1, length = 32 bytes)

The parameters are fixed by the protocol and are never negotiated, so a fake
server cannot ask for a weaker hash. The salt is fixed because a console knows
only an address and a code before its first message, and the address it typed may
not be the one another console typed. The server works out `K` once, when the code
is made, and keeps it beside the code. Checking a guess at the server therefore
costs the guesser the slow hash and the server almost nothing.

## Pairing

`Noise_XXpsk0_25519_ChaChaPoly_SHA256` ([the Noise spec](https://noiseprotocol.org/noise.html)),
with `K` as the pre-shared key and the prologue `cabinet-linkup pair 1`. The side
that has the code and is joining is the initiator. Each side has one long-term
X25519 key: a console makes its own the first time LinkUp is turned on, a server
the first time it starts.

    -> psk, e                 Pair, first. Empty payload.
    <- e, ee, s, es           Answer. Payload: the server's hello.
    -> s, se                  Pair, second. Payload: who is joining.
    <- (transport message)    Sealed reply: the result.

- **Message 1 is the joiner's proof.** It ends in an authentication tag under a
  key that depends on `K`. The server tries it against each waiting code's `K`
  (cheap, since `K` is already worked out) and answers 403 if none fits. A stranger
  without a code gets no key, no salt and no sign of what the server holds.
- **Message 2 is the server's proof.** Its static key and payload are encrypted
  under a key that depends on `K` and on both ephemeral keys, so a fake server
  cannot make one, and a console that cannot open it stops and keeps nothing.
- **Message 3 binds the joiner's key.** The server checks it, then uses up the code
  under a lock, so the first to finish takes the place.
- **Both long-term keys are hashed into the handshake,** so nobody can swap either
  without `K`. Each side remembers the other's key only after the sealed reply.

A pairing in progress lives 60 seconds in memory, at most 32 at a time, oldest
dropped first. At most 200 codes wait at once.

**Rate limit.** Failed Pair attempts count, per source address and in total: 5 a
minute per address, 30 a minute in all, then 429. Behind a reverse proxy all sources
look alike, so the total is what holds there. Failures are logged as counts only.

**Offline guessing.** Whoever captures message 1, or a fake server that receives
it, can try guesses at leisure, at one Argon2id per guess, over about 2^57.7 codes.
A code is good for 7 days at most and is used up the moment the real pairing
completes, so a guess only helps someone who blocks the real pairing and finishes
the search inside those 7 days.

### Who is joining

The payload of message 3 is JSON. A console:

    {"kind": "console", "protocol": 1, "oldest": 1, "stream": 1,
     "account": "<stable id>", "username": "alex"}

`account` is a string the console picks that stays the same for that account on
that console (RomM's user id). The server keeps it to recognise the account after
a reinstall, and shows only `username`.

A server, linking:

    {"kind": "server", "protocol": 1, "oldest": 1,
     "name": "Riley's", "address": "linkup.example.org:7845",
     "account": {"id": "<person id>", "username": "maya", "key": "<base64>", "stream": 1}}

The reply is sealed under the transport keys, nonce 0 from responder to initiator:

    console: {"ok": true, "console": 3}
    server:  {"ok": true, "name": "Alex's", "account": {"id": ..., "username": ..., "key": ..., "stream": ...}}

## Sessions

Once paired, every connection opens a session. The address can change, because the
key is what is checked, so a retyped address needs no new pairing.

`Noise_IK_25519_ChaChaPoly_SHA256`, prologue `cabinet-linkup session 1`. The
initiator already holds the responder's remembered key.

    -> e, es, s, ss           Open. Payload: {"protocol":1,"oldest":1,"stream":1}
    <- e, ee, se              Answer. Payload: {"session":"<16 hex>","protocol":1,"oldest":1}

- **An unknown key gets 404,** the same as a malformed body. The console port holds
  nothing for a stranger.
- **A key that was removed gets a real answer:** the payload is `{"removed": true}`
  and no session opens. Because it is inside the handshake it is authenticated, so
  a fake server cannot make a console say "Removed from the LinkUp server".
- **Message 1 can be replayed by someone who recorded it, but it does no harm:** its
  payload holds only version numbers and the replayer cannot read the answer. A new
  session replaces the old only once its first Sealed request arrives, so a replay
  cannot push a console off. The server keeps at most one unconfirmed session per key.

A Sealed request:

    session (8) | counter (8, big endian) | ChaCha20-Poly1305(key = sender's transport key,
                                                              nonce = counter,
                                                              ad = 4 | session | counter)

Counters start at 1 and only go up. Requests can overtake each other (a long request
and a short one), so the server keeps a window of the last 64 and refuses a repeat or
anything older, with 404. The answer uses the other transport key, the same counter
as its nonce, and `ad = 5 | session | counter`, so an answer cannot be moved to a
different request, and each nonce is used once under each key because a request
is answered once.

A session closes after 120 seconds with no request. Any answer that is not 200 means:
open a new session, with a growing wait between tries, up to 60 seconds. A restart of
the server closes every session.

## Between two consoles: sealed boxes

Friends' keys come from the server. What the server only carries (the invite's game
name, and the connection details) is sealed for the other console, in the one-way
pattern `Noise_K_25519_ChaChaPoly_SHA256`, with the prologue
`cabinet-linkup box 1 ` followed by the invite's id:

    -> e, es, ss              Payload: JSON the two consoles agree on.

The server sees base64 and passes it on. The first box of an invite states
`stream`. A console drops a box whose prologue or sender does not fit.

**The limit.** A console learns a friend's key from its server. A server that has been
broken into could hand out its own key and read the boxes; the two consoles do not
check each other's keys another way.

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
| `invite` | `friend`, `sealed` | `{"invite": "<id>", "expires_in": 1800}`, or `unavailable` |
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

**`wait`** is the one long request, held up to 25 seconds, and it is how a console counts
as on: a console with no request for 75 seconds shows Offline. `after` is the last `next`
it was given, and `epoch` changes when the server restarts, which means fetch `friends`
again and start over at 0.

**Events:**

| `type` | Fields | |
|---|---|---|
| `friends` | | Something on the list changed; fetch it again. It does not say what. There is no event for someone coming online and no way to nudge anyone. |
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
session ends.

**`report` reasons,** the only words that reach the log: `none`, `no_path`, `timeout`,
`relay_limit`, `behind`, `other`.

## Between servers

Server to server is the same protocol on the same port: a server pairs with a link code as
a joiner (`kind: "server"`) and opens sessions with the same `Open`. Each side remembers
the other's key and address, and each sends to the other by its own short requests. There
is no long request between servers.

Only people named in a pair are ever mentioned across a link. A server refuses any message
about anyone else, and nothing is passed on to a third server.

| `op` | Fields | |
|---|---|---|
| `link_hello` | `name`, `address`, `pairs`: `[{"ours", "theirs", "status", "username", "key", "stream"}]` | Sent every 60 seconds and when something changes. It carries the sender's current name and address, which the other side saves, so a changed address reaches a linked server on its own. |
| `link_pair_remove` | `theirs`, `ours` | A pair was removed on the other side. |
| `link_unlink` | | The whole link was removed on the other side. |
| `link_invite` | `invite`, `from`, `to`, `sealed` | Answer: ok or `unavailable`. |
| `link_invite_ended` | `invite`, `reason` | |
| `link_join` | `invite` | Answer: ok or `unavailable`. The inviter's server decides. |
| `link_signal` | `session`, `sealed` | |
| `link_end` | `session` | |
| `link_relay` | `session` | The same answer as `relay`. |

## The relay

A TURN server in the container, off until its owner turns it on, on UDP (3478 and a small
range, set by the owner). The host's server's relay is used first, and the friend's if the
host's is off.

A `relay` password belongs to one session. A new connection can use it for 10 minutes after
it is given out. A connection that has already started keeps working until the session
ends, at most 24 hours. It is refused as soon as the server has seen the session end.

It refuses to relay to private addresses: IPv4 and IPv6 loopback, private, link-local,
unique-local, carrier-grade (100.64.0.0/10), multicast and unspecified addresses, and
IPv4 addresses mapped into IPv6.

Each connection is capped at 26 Mbit/s, about what the console's stream reaches at its
short peaks. Bytes are counted through the relay, saved now and then, and when the month's
limit is reached the relay refuses everything until its reset day. Game packets stay
encrypted between the two consoles, by WebRTC, so the relay cannot read them.

## What a hacked server gets

Usernames, who is friends with whom, the owner's notes, console and server public keys, the
addresses of consoles connected at that moment, use of the relay's data, and the chance to
send false connection details or false keys to consoles. It never holds a RomM login, a game,
a save or play history, and it cannot read a box that was sealed before it was broken into.
The console's LinkUp program is walled off from the home folder that holds the RomM token and
the PIN.
