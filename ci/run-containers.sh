#!/bin/sh
# Two linked containers from the real image, and stand-in consoles against them: pairing, a game
# across the link, and a container with no shell and nothing to write to. It starts the containers
# on a network of their own and removes everything when it ends.
#
#   ci/run-containers.sh <image> [docker|podman]
set -eu

image=$1
engine=${2:-docker}
net=linkup-check-$$
work=$(mktemp -d)
standin=$work/standin

fail() { echo "FAILED: $1" >&2; exit 1; }

cleanup() {
    $engine logs linkup-a-$$ 2>/dev/null | tail -20 || true
    $engine rm -f linkup-a-$$ linkup-b-$$ >/dev/null 2>&1 || true
    $engine network rm "$net" >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT

CGO_ENABLED=0 go build -o "$standin" ./cmd/standin

$engine network create "$net" >/dev/null
for name in a b; do
    $engine run -d --name linkup-$name-$$ --network "$net" --network-alias linkup-$name \
        --read-only --cap-drop=ALL --security-opt=no-new-privileges \
        -p 127.0.0.1::7845/tcp -p 127.0.0.1::7846/tcp \
        "$image" >/dev/null
done

port() { $engine port linkup-$1-$$ "$2/tcp" | head -1 | sed 's/.*://'; }
console_a=127.0.0.1:$(port a 7845)
console_b=127.0.0.1:$(port b 7845)
admin_a=http://127.0.0.1:$(port a 7846)
admin_b=http://127.0.0.1:$(port b 7846)

for url in "$admin_a" "$admin_b"; do
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        curl -fs -o /dev/null "$url/static/mark.svg" && break
        sleep 1
    done
done

# The admin page, as a browser would: a password, a name and an address, then a code.
page() { # jar url path [form...]
    jar=$1; url=$2; path=$3; shift 3
    curl -s -o "$work/out.html" -D "$work/headers" -b "$jar" -c "$jar" "$@" "$url$path"
}
setup() { # jar url name address
    page "$1" "$2" /setup/password -d password=correct-horse -d again=correct-horse
    page "$1" "$2" /setup/address -d "name=$3" --data-urlencode "address=$4"
    page "$1" "$2" /setup/relay/skip
}
own_code() { # jar url local-address
    page "$1" "$2" /setup/own --data-urlencode "address=$3"
    where=$(sed -n 's/^[Ll]ocation: *//p' "$work/headers" | tr -d '\r')
    page "$1" "$2" "$where"
    grep -o '[A-Z0-9]\{4\}-[A-Z0-9]\{4\}-[A-Z0-9]\{4\}' "$work/out.html" | head -1
}

setup "$work/a.jar" "$admin_a" "Alex's" "linkup-a:7845"
setup "$work/b.jar" "$admin_b" "Rileys" "linkup-b:7845"

code_a=$(own_code "$work/a.jar" "$admin_a" "$console_a")
code_b=$(own_code "$work/b.jar" "$admin_b" "$console_b")

"$standin" pair "$work/maya" "$console_a" "$code_a" acct-maya maya >/dev/null || fail "pairing maya"
"$standin" pair "$work/june" "$console_b" "$code_b" acct-june june >/dev/null || fail "pairing june"
"$standin" signin "$work/maya" acct-maya maya >/dev/null
"$standin" signin "$work/june" acct-june june >/dev/null

"$standin" pair "$work/wrong" "$console_a" "K7MR-4QXT-9HPV" a a 2>/dev/null && fail "a wrong code paired"

# The consoles' people, by the option the admin page offers.
person() { # jar url
    page "$1" "$2" /links/make
    grep -o '<option value="[0-9a-f]*"' "$work/out.html" | head -1 | sed 's/.*value="//; s/"//'
}
maya=$(person "$work/a.jar" "$admin_a")
june=$(person "$work/b.jar" "$admin_b")
[ -n "$maya" ] && [ -n "$june" ] || fail "no accounts on the admin page"

page "$work/a.jar" "$admin_a" /links/make -d "for=$maya"
where=$(sed -n 's/^[Ll]ocation: *//p' "$work/headers" | tr -d '\r')
page "$work/a.jar" "$admin_a" "$where"
link_code=$(grep -o '[A-Z0-9]\{4\}-[A-Z0-9]\{4\}-[A-Z0-9]\{4\}' "$work/out.html" | head -1)
[ -n "$link_code" ] || fail "no link code"
page "$work/b.jar" "$admin_b" /links/enter --data-urlencode "address=linkup-a:7845" -d "code=$link_code" -d "for=$june"
grep -qi '^location: /links' "$work/headers" || { cat "$work/out.html"; fail "linking the servers"; }

online() { "$standin" friends "$1" | grep -q '"status":"online"'; }
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    online "$work/maya" && online "$work/june" && break
    sleep 1
done
online "$work/maya" || fail "maya never saw june online across the link"
online "$work/june" || fail "june never saw maya online across the link"
echo "pairing and linking work"

friend=$("$standin" friends "$work/maya" | sed 's/.*"id":"\([^"]*\)".*/\1/')
invite=$("$standin" invite "$work/maya" "$friend" sealed-by-maya | sed 's/.*"invite":"\([^"]*\)".*/\1/')
[ -n "$invite" ] || fail "inviting"
"$standin" join "$work/june" "$invite" >/dev/null || fail "joining"
"$standin" report "$work/maya" "$invite" direct none || fail "reporting"
"$standin" end "$work/june" "$invite" || fail "ending"
echo "a game across the link works"

# The container itself: no shell, and it cannot write outside its data folder.
if $engine exec linkup-a-$$ /bin/sh -c true 2>/dev/null; then fail "the container has a shell"; fi
echo "the container has no shell"

# The console port holds nothing for a stranger.
status=$(curl -s -o /dev/null -w '%{http_code}' "http://$console_a/")
[ "$status" = 404 ] || fail "the console port answered $status to a stranger"

# Nothing private in the log.
log=$($engine logs linkup-a-$$ 2>&1)
for word in maya june "$code_a" "$link_code" sealed-by-maya; do
    if printf '%s' "$log" | grep -q "$word"; then fail "$word is in the log"; fi
done
echo "the log holds nothing private"
echo "all good"
