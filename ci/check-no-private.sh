#!/usr/bin/env bash
#
# No address, host name, password or home path of anybody's machines goes in a tracked file (CLAUDE.md).
# This repository is public, so this reads every tracked file for a private network address or a
# person's home folder, prints each as file:line, and fails if there is any. It is 2P Start's
# check-no-private.sh, with this repository's examples.
#
#   ci/check-no-private.sh
#
# Telling a home address from a public one is this server's job, so its code, its tests and its docs
# name private addresses on purpose. A network block (`100.64.0.0/10`) is never one machine, and is
# allowed. One address is allowed only if EXAMPLES lists it: a stand-in that is nobody's machine. A new
# one goes on the list in the pull request that needs it, where the person reviewing sees it.
set -euo pipefail

EXAMPLES=(
    192.168.1.50                    # the server at home, in the README and on the first-run page
    192.168.1.1 192.168.1.5 192.168.1.9 192.168.1.10 192.168.0.1 192.168.0.2
    10.0.0.1 10.1.2.3
    172.16.0.1 172.16.0.9 172.31.255.255
    172.17.0.1                      # Docker's own bridge, as a container sees a console
    100.64.0.1 100.127.255.254 100.127.255.255 100.100.1.2
)

cd "$(git rev-parse --show-toplevel)"
export ALLOWED="${EXAMPLES[*]}"
found=0
while IFS= read -r -d '' file; do
    [ -f "$file" ] || continue
    if ! perl -ne '
        BEGIN { %ok = map { $_ => 1 } split " ", $ENV{ALLOWED}; $bad = 0 }
        while (/(?<![\w.])(10(?:\.\d{1,3}){3}|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2}|192\.168(?:\.\d{1,3}){2}|100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])(?:\.\d{1,3}){2})(?![\w]|\.\d)(\/\d)?/g) {
            next if defined $2 || $ok{$1};
            print "$ARGV:$.: $1, a private address\n";
            $bad = 1;
        }
        if (m{/(?:Users|home)/(?!runner\b)[A-Za-z]} || /[A-Za-z]:\\Users\\[A-Za-z]/) {
            print "$ARGV:$.: a home folder\x27s path\n";
            $bad = 1;
        }
        END { exit $bad }
    ' "$file"; then
        found=1
    fi
done < <(git ls-files -z)

if [ "$found" -ne 0 ]; then
    echo "FAILED: a private address or a home folder's path is in a tracked file." >&2
    echo "An address that is only an example, and nobody's machine, goes on EXAMPLES in ci/check-no-private.sh." >&2
    exit 1
fi
echo "ok: nothing private in a tracked file"
