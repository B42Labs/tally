#!/bin/sh
# Maps a release tag to the version the Debian package carries. The release
# workflow reads it as `sh packaging/release-version.sh "${GITHUB_REF_NAME}"`,
# and it is the one place the mapping is written: a second copy in YAML would
# drift from the one packaging/release_test.go covers.
#
# The hyphen of a prerelease tag becomes a tilde, because dpkg reads everything
# after a hyphen as the package revision and would sort 1.2.3-rc.1 above 1.2.3,
# while a tilde sorts below every other character. The accepted prerelease part
# carries no hyphen of its own, so exactly one can be present and the
# substitution is unambiguous.
set -e

if [ $# -ne 1 ]; then
    printf 'release-version: usage: release-version.sh <tag>\n' >&2
    exit 1
fi

tag=$1
version=${tag#v}

# A tag without the leading v leaves $version equal to $tag, which is the first
# test; the second is the shape of what is left.
if [ "$version" = "$tag" ] || ! printf '%s' "$version" | grep -Eq \
    '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.]+)?$'; then
    printf "release-version: '%s' is not a release tag; expected vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-PRERELEASE\n" "$tag" >&2
    exit 1
fi

printf '%s\n' "$version" | tr '-' '~'
