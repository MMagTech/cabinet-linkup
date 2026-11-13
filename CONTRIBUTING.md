# Contributing to cabinet-linkup

## The map

| Path | Its job |
|---|---|
| `cmd/cabinet-linkup/main.go` | Starts everything |
| `cmd/standin/main.go` | A stand-in console on the command line, for trying a real server by hand |
| `internal/server` | Puts the parts together, apart from main so tests can run real servers side by side |
| `internal/proof` | The pairing proof, sessions and sealed boxes: every use of Noise and of the slow hash |
| `internal/store` | The data folder, in JSON files written atomically |
| `internal/logfile` | The capped log file, and what may never go in it |
| `internal/console` | What consoles talk to: pairing, presence, invites, connection details, one long request at a time |
| `internal/link` | Server to server |
| `internal/relay` | Pion, per-game passwords, the GB counter, the private address block |
| `internal/admin` | The admin page, with `html/template` and no JavaScript framework |
| `internal/standin` | A stand-in console and what the tests share: it speaks PROTOCOL.md as the real one must |
| `internal/update` | The update notice, asking GitHub |
| `ci/run-containers.sh` | Two linked containers and stand-in consoles against the real image |

Each part owns its job and no other. Only `internal/admin` draws a page, only `internal/proof` touches
keys, and only `internal/logfile` writes the log.

## Building and testing

You need Go, at the version in `go.mod`.

```bash
go test -race ./...
go vet ./...
```

The image, and two linked containers with stand-in consoles against it:

```bash
docker build -t cabinet-linkup:dev .
./ci/run-containers.sh cabinet-linkup:dev
```

CI runs all of this on every pull request, with `govulncheck` and a build for amd64 and arm64.

Consoles on one home network can only reach a relay that may send to private addresses, which a real
server refuses. `LINKUP_TEST_RELAY_PRIVATE=1` allows it, and the log says so. It is for test servers only
and stays out of the setup guide.

## Trying it, then releasing it

There is one branch, `main`. An image's tag is its channel:

| Tag | Published | Who runs it |
|---|---|---|
| `:latest` and `:<version>` | On a `v*` tag | Owners. The compose files and the Unraid template follow `:latest` |
| `:testing` | On every push to `main`, once Checks pass | The maintainer's own server, before a release |

`.github/workflows/release.yml` builds and signs both, and nothing else publishes. It pushes the image
with no tag, signs it and checks the signature, and only then moves the tag to it, so a failure leaves
the tag on the last image that was signed and checked. A `:testing` server's
version is `testing-` and the commit's first seven characters, on the Server tab and in the log, and it
shows no update notice. When pushes land close together, only the newest becomes `:testing`.

A release goes like this:

1. Merge to `main`, and wait for Release in Actions to publish `:testing`.
2. Copy your server's data folder, then switch the server to `:testing`. On Unraid: Docker, the
   container's Edit, the Repository field ending in `:testing` instead of `:latest`, Apply.
3. Use it with real consoles. A fix is another merge, and Apply again pulls the newest `:testing`.
4. When it is right, tag the commit the Server tab shows, `git tag v0.1.2 <commit>`, and push the tag.
   Then switch back to `:latest`, which is now the same code.
5. Once Release in Actions has published `:latest`, make a GitHub Release for the tag with a few lines
   on what changed: `gh release create v0.1.2 --notes "..."`. The update notice on owners' admin pages
   asks GitHub for the latest Release, not the latest tag, so without one nobody sees it.

### Going back to `:latest`

Going back is changing the tag again, on the same folder, while `Format` in `internal/store` stays the
same. An older server reads a newer one's folder and can forget what it does not know, such as a new
setting, which then starts from its default when `:testing` runs again.

Raise `Format` only when an older server would misread the file, never to add a field, and say so in the
pull request's title. Once `:testing` has run on a raised `Format`, `:latest` refuses the folder rather
than overwrite it, and going back means putting the copy from step 2 back.

## The rules

CabinetOS's own rules apply here: search before you write, remove what you replace, no commented-out
code, docs change with the code, and decisions in small files. These are this repository's:

- **`PROTOCOL.md` changes with the code,** in the same pull request, and changes only add: never rename
  or remove. A version number moves only on a real break.
- **Nothing from the network reaches a log line.** The log takes numbers and fixed words only. An owner
  pastes it into a public issue, so no code, password, key, internet address, username or game title.
- **The server never holds a RomM login, a game, a save or play history.** Nothing here needs one.
- **No address, host name, password or home path of anybody's machines in a tracked file.**
- **No new dependency without saying why** in the pull request. Every one is more to patch.
