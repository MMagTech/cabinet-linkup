# cabinet-linkup, for coding sessions

The self-run server behind LinkUp on CabinetOS. One small Go program with Pion's relay inside it, in a
nearly empty container. The console's side is in the CabinetOS repository, and shares no code with this
one; [PROTOCOL.md](PROTOCOL.md) is the one definition of what they say to each other.

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It has the map of the code, how to build and test, and
the rules. CabinetOS's own rules also apply, in that repository's `CONTRIBUTING.md`, "The rules", and its
`CLAUDE.md`, "Rules for every change". [AGENTS.md](AGENTS.md) is the same for every AI tool.

LinkUp's decisions, the server's among them, are in CabinetOS's `docs/decisions/`, in the LinkUp section
of its `README.md`. LinkUp was planned on MMagTech/cabinetos#654, "LinkUp: play a game with a friend on
another CabinetOS console over the internet", closed on 2026-11-10. Name an issue or pull request in
CabinetOS as `MMagTech/cabinetos#N`, with its title: a bare `#N` links to this repository's own.

No address, host name, password or home path of anybody's machines goes in a tracked file.
