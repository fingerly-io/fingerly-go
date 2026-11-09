#!/usr/bin/env bash
# The version gate release.yml runs. The Version constant in fingerly.go is the
# manifest: a Go module's version is its git tag, and the tag is cut from this.
#
#   version      print the manifest's version
#   consistency  fail if the tag this version makes could not be a release
#   check        print version= and release=true|false for $GITHUB_OUTPUT
#   index        ask the Go proxy for the tagged version until it has it
set -euo pipefail

die() {
  echo "release-version: $*" >&2
  exit 1
}

# Only a clear yes or no is an answer. An outage has to fail the job, because
# reading it as "not published" would try to release a version that exists.
lookup() {
  local out
  out=$(curl --silent --show-error --location --max-time 30 --retry 3 --retry-all-errors \
    --write-out '\n%{http_code}' "$1") || die "could not reach $1"
  status=${out##*$'\n'}
  body=${out%$'\n'*}
}

# The list, never this version's .info: the proxy remembers a not-found for a
# version for up to half an hour, so asking before the tag exists would make
# the request after tagging fail.
on_registry() {
  lookup "https://proxy.golang.org/$module/@v/list"
  case "$status" in
    200) grep -Fxq "v$version" <<<"$body" ;;
    404 | 410) return 1 ;;
    *) die "the Go proxy answered $status" ;;
  esac
}

tag_exists() {
  local rc=0
  git ls-remote --exit-code --tags origin "refs/tags/v$version" >/dev/null || rc=$?
  case "$rc" in
    0) return 0 ;;
    2) return 1 ;;
    *) die "git ls-remote failed with $rc" ;;
  esac
}

module=$(awk '$1 == "module" { print $2; exit }' go.mod)
[ -n "$module" ] || die "no module path in go.mod"

version=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' fingerly.go)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no X.Y.Z version in the manifest: '$version'"

case "${1:-}" in
  version)
    echo "$version"
    ;;
  consistency)
    # From v2 on, Go requires the major version in the module path. A tag
    # without it is one the proxy will not serve as that version.
    major=${version%%.*}
    if [ "$major" -ge 2 ] && [[ "$module" != */v"$major" ]]; then
      die "v$version needs the module path to end in /v$major, go.mod has $module"
    fi
    ;;
  check)
    echo "version=$version"
    if tag_exists; then
      echo "v$version is already tagged" >&2
      echo "release=false"
    elif on_registry; then
      echo "$version is already on the Go proxy" >&2
      echo "release=false"
    else
      echo "release=true"
    fi
    ;;
  index)
    url="https://proxy.golang.org/$module/@v/v$version.info"
    for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
      lookup "$url"
      if [ "$status" = 200 ]; then
        jq -e --arg v "v$version" '.Version == $v' <<<"$body" >/dev/null || die "$url answered $body"
        echo "indexed: $body"
        exit 0
      fi
      echo "attempt $attempt: $status" >&2
      sleep 15
    done
    die "the tag exists but $url still answers $status: request it again"
    ;;
  *)
    die "usage: release-version.sh version|consistency|check|index"
    ;;
esac
