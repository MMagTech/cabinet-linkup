# cabinet-linkup, for coding sessions

The self-run server behind LinkUp on CabinetOS. One small Go program with Pion's relay inside it, in a
nearly empty container. The console's side is in the CabinetOS repository, and shares no code with this
one; [PROTOCOL.md](PROTOCOL.md) is the one definition of what they say to each other.

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. It has the map of the code, how to build and test, and
the rules. CabinetOS's own rules also apply, in that repository's `CONTRIBUTING.md`, "The rules", and its
`CLAUDE.md`, "Rules for every change". LinkUp's plan and every decision are on its issue #654.

No address, host name, password or home path of anybody's machines goes in a tracked file.
