#!/usr/bin/env python3
"""
Does a doc or a comment name a file that is not there?

  ci/check-named-files.py

Reads every tracked doc and the comments in every tracked source file, prints each file named that
is not there as file:line, and exits 1 if there is any; 0 when there is none. .github/workflows/
checks.yml runs it on every pull request; on GitHub each is also marked on the pull request.

WHY. A comment or a doc that names a file which moved reads exactly like one that is right, and
CabinetOS's audit found 33 of them after its restructure (MMagTech/cabinetos#398, "Audit the code and
restructure it for contributors (main.cpp is 20,000 lines)"): the rules that held were the ones CI
checks. This is 2P Start's check-named-files.py, cut to this repository's Go.

WHAT IS READ:
  * in a doc (`.md`), a link that is not a web address or an anchor: its target, from the doc's own
    folder, must be there;
  * in a doc, a path in backticks, and in a comment any path, that starts with one of this
    repository's top-level folders (`internal/relay/guard.go`, `ci/run-containers.sh`), with a line
    number after a colon allowed, must be there;
  * in a comment, a bare file name of this repository's kinds (`pairing.go`, `checks.yml`,
    `PROTOCOL.md`) must be a file here. A bare `.go` name in a Go file names a file of its own
    package, so it must be in the same folder.
A path is also there when .gitignore covers it, or when SKIP names it, with why. Code blocks in docs
are not read.

ANOTHER REPOSITORY'S FILES. CabinetOS and 2P Start have folders named like these (`docs/`, `ci/`),
and this check cannot see them. A path or a name said to be theirs, right after "CabinetOS's", "2P
Start's", "the console's" or "that repository's" and at most three words more, is not read.
"""

import os
import posixpath
import re
import subprocess
import sys
import urllib.parse

# What looks like ours and is not in the tree. Each says why.
SKIP = set()

# What has comments, and how they are written.
SLASH = (".go",)
HASH = (".sh", ".py", ".yml", ".yaml")
HASH_NAMES = ("Dockerfile",)

FENCE = re.compile(r"^\s*(```|~~~)")
CODE = re.compile(r"(?<!`)(`+)(?!`)(.+?)(?<!`)\1(?!`)", re.S)
PARAGRAPH = re.compile(r"(?:[^\n]+\n?)+")
LINK = re.compile(r"\]\(\s*<?([^)\s>]+)>?(?:\s+[\"'(][^)]*)?\)")
REFERENCE = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?", re.M)
SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")
LINE_NUMBER = re.compile(r":\d+(-\d+)?$")
NOT_A_PATH = re.compile(r"[\s*?<>{}\[\]$|=()'\",;\\]")
# Go's comments, and the strings and runes that can hold what looks like one ("https://").
GO_TOKEN = re.compile(
    r"//[^\n]*"
    r"|/\*.*?(?:\*/|\Z)"
    r"|`[^`]*`?"
    r"|\"(?:\\.|[^\"\\\n])*\"?"
    r"|'(?:\\.|[^'\\\n])*'?", re.S)
HASH_COMMENT = re.compile(r"(?:^|(?<=\s))#(?![!\[{(])[^\n]*", re.M)
PATH = re.compile(r"(?<![\w/.~$@:%-])(?:\./)?(\.?[A-Za-z_][\w.+-]*(?:/[\w.+-]+)+/?)"
                  r"(?![\w*<{$%(\[]|/)")
BARE = re.compile(r"(?<![\w/.@$%-])([\w+-][\w.+-]*\.(?:go|sh|py|yml|yaml|md))(?![\w/-])")
# Said of a file that has gone, and true of it: "moved out of handler.go".
HISTORY = re.compile(r"\b(?:out of|was in|were in|old|formerly|used to|moved|split|renamed|replaced)"
                     r"\b.{0,30}$", re.I)
# Said to be another repository's: "CabinetOS's `docs/decisions/`".
THEIRS = re.compile(r"\b(?:CabinetOS|2P Start|the console|that repository)['’]s\s+(?:\S+\s+){0,3}"
                    r"[`(\[]*$", re.I)


def git(*args):
    return subprocess.run(["git", *args], capture_output=True, text=True, check=True).stdout


def listed():
    """Tracked files, and new ones not yet added that .gitignore does not cover."""
    out = git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
    return sorted({f for f in out.split("\0") if f})


def line_of(text, at):
    return text.count("\n", 0, at) + 1


def comments(path, text):
    """(offset, comment) for each comment in a source file."""
    name = posixpath.basename(path)
    if name.endswith(SLASH):
        return [(m.start(), m.group(0)) for m in GO_TOKEN.finditer(text)
                if m.group(0).startswith(("//", "/*"))]
    if name.endswith(HASH) or name in HASH_NAMES:
        return [(m.start(), m.group(0)) for m in HASH_COMMENT.finditer(text)]
    return []


def main():
    os.chdir(git("rev-parse", "--show-toplevel").strip())
    ours = listed()
    files = set(ours)
    folders = set()
    for f in files:
        while "/" in f:
            f = f.rsplit("/", 1)[0]
            folders.add(f)
    top = {f.split("/", 1)[0] for f in ours if "/" in f}
    names = {posixpath.basename(f) for f in files}

    def there(path, where, bare):
        if path in SKIP:
            return True
        if not bare:
            path = path.rstrip("/")
            return path in files or path in folders
        if where.endswith(".go") and path.endswith(".go"):
            return posixpath.join(posixpath.dirname(where), path) in files
        return path in names

    def a_path(span):
        span = LINE_NUMBER.sub("", span.strip()).rstrip(".")
        if span.startswith("./"):
            span = span[2:]
        if NOT_A_PATH.search(span) or "/" not in span:
            return None
        if span.split("/", 1)[0] not in top or "/../" in "/" + span + "/":
            return None
        return span.rstrip("/")

    named = []  # (file, line, what, the path or name it was read as, a bare name)
    for doc in sorted(f for f in ours if f.endswith(".md")):
        with open(doc, encoding="utf-8") as fh:
            text = fh.read()
        lines = text.split("\n")
        inside = False
        for i, line in enumerate(lines):
            if FENCE.match(line):
                inside = not inside
                lines[i] = ""
            elif inside:
                lines[i] = ""
        text = "\n".join(lines)
        spans = []
        for p in PARAGRAPH.finditer(text):
            for m in CODE.finditer(text, p.start(), p.end()):
                spans.append(m)
                path = a_path(m.group(2))
                if path and not THEIRS.search(text[p.start():m.start()]):
                    named.append((doc, line_of(text, m.start()), "`" + path + "`", path, False))
        for m in spans:
            text = text[:m.start()] + re.sub(r"[^\n]", " ", text[m.start():m.end()]) + text[m.end():]
        for m in list(LINK.finditer(text)) + list(REFERENCE.finditer(text)):
            target = m.group(1)
            if SCHEME.match(target) or target.startswith("#"):
                continue
            path = urllib.parse.unquote(re.split(r"[#?]", target, maxsplit=1)[0])
            if not path:
                continue
            path = posixpath.normpath(posixpath.join(posixpath.dirname(doc), path))
            named.append((doc, line_of(text, m.start(1)), "a link to " + target, path, False))

    for source in sorted(ours):
        if not os.path.isfile(source):
            continue
        try:
            with open(source, encoding="utf-8") as fh:
                text = fh.read()
        except UnicodeDecodeError:
            continue
        for at, comment in comments(source, text):
            for m in PATH.finditer(comment):
                path = a_path(m.group(1))
                if path and not THEIRS.search(comment[:m.start()]):
                    named.append((source, line_of(text, at + m.start()), path, path, False))
            for m in BARE.finditer(comment):
                name = m.group(1)
                before = comment[:m.start()]
                if HISTORY.search(before) or THEIRS.search(before) or before.endswith("/"):
                    continue
                named.append((source, line_of(text, at + m.start()), name, name, True))

    missing = [n for n in named if not there(n[3], n[0], n[4])]
    if missing:
        asked = sorted({n[3] for n in missing if not n[4]})
        ignored = subprocess.run(["git", "check-ignore", "--no-index", "--stdin"],
                                 input="\n".join(asked) + "\n", capture_output=True,
                                 text=True).stdout.split("\n")
        ignored = {p.rstrip("/") for p in ignored if p}
        missing = [n for n in missing if n[4] or n[3] not in ignored]

    on_github = bool(os.environ.get("GITHUB_ACTIONS"))
    for where, line, what, _, bare in missing:
        kind = " in this folder" if bare and where.endswith(".go") and what.endswith(".go") else ""
        print(f"{where}:{line}: {what}, which is not there{kind}")
        if on_github:
            print(f"::error file={where},line={line}::{what}, which is not there{kind}. Name the "
                  "file where it is now, or see ci/check-named-files.py.")
    if not missing:
        print(f"Every file named is there ({len(named)} read).")
        return 0
    print(f"{len(missing)} named file(s) not there.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
