# For AI coding tools

cabinet-linkup is the self-run server behind LinkUp on CabinetOS: one small Go program with Pion's
relay inside it, in a nearly empty container. A person reviews every pull request. Read these before
changing anything:

1. `CONTRIBUTING.md`: the map of the code, building and testing, and "The rules", which apply to every
   tool.
2. `PROTOCOL.md`: what a console and this server say to each other. It is the one definition, it
   changes with the code, and it only adds.
3. CabinetOS's `docs/decisions/`, the LinkUp section of its `README.md`: what was decided and why,
   https://github.com/MMagTech/cabinetos/blob/main/docs/decisions/README.md. Look there before
   proposing a change.

## Before you write code

- **A small fix needs no issue:** a few lines that put back what was meant to work.
- **Anything new needs an issue MMagTech has agreed to first**, before the work: a feature, a setting,
  a change to `PROTOCOL.md`, a new file or package, a new dependency, a change to the build or CI, a
  refactor across files. Ask your person to open it here; you may draft it.
- **The console's side is CabinetOS's** and shares no code with this one. A change both sides need is
  a pull request in each, and `PROTOCOL.md` says what changes between them.
- **A decision between real options** is a small file in CabinetOS's `docs/decisions/`, in a CabinetOS
  pull request beside this one (`CONTRIBUTING.md`, "The rules").

## Also

- This repository is public: no address, host name, password or home path of any machine goes in a
  file. A made-up private address for a test or an example goes on the list in
  `ci/check-no-private.sh`, in the same pull request.
- Name an issue or pull request by its title as well as its number. One in CabinetOS is
  `MMagTech/cabinetos#N`: a bare `#N` here links to this repository's own.
- The person who sends a pull request answers for it.

## Before you say it is done

- `go test -race ./...`, `go vet ./...`, and staticcheck as `.github/workflows/checks.yml` runs it.
- `ci/check-no-private.sh`, `python3 ci/check-named-files.py` and `ci/check-long-files.sh`.
- List every file you created, deleted or moved, and check the map in `CONTRIBUTING.md` still matches.
  If a check fails, say so; never work around it.
