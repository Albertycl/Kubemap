#!/bin/sh
# make release VERSION=x.y.z: everything an install needs, in dist/, from a
# clean checkout:
#
#   kubemap-<v>-image.tar.gz   the image with the server and the node agent
#                              (docker save), for "k3s ctr images import" while
#                              there is no registry
#   kubemap-<v>.yaml           the manifests for that image (kubectl apply -f)
#   IMAGE.txt                  image tag and ID
#   RELEASE.txt                version, commit, toolchain
#   SHA256SUMS                 checksums of all of the above
#
# Steps: check the inputs, make all (fmt, vet, tests, web UI, binaries), check
# the version in both binaries, make smoke, build and check the image, write
# dist/. Any failure stops the release. The Makefile passes VERSION, IMAGE,
# GO and MAKE. SKIP_IMAGE=1 leaves the image out (a machine without Docker);
# RELEASE.txt then says so and no image files are written.
set -eu

version=${VERSION:?}
image=${IMAGE:?}
make=${MAKE:-make}
skip_image=${SKIP_IMAGE:-0}
dist=dist

fail() {
	echo "release: FAIL: $*" >&2
	exit 1
}
step() { echo "==> $*"; }

step "check inputs"
echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
	fail "VERSION=$version is not a release version; use x.y.z, for example: make release VERSION=0.1.0"
[ -z "$(git status --porcelain)" ] ||
	fail "the checkout has uncommitted changes (git status); a release is built only from a clean checkout"
[ -f web/package.json ] ||
	fail "the web UI sources (web/package.json) are not in this checkout, so the server would ship without its UI. Build the release from a branch that has the web UI (KM-20 and later)."
command -v npm >/dev/null || fail "npm is needed to build the web UI"
if [ "$skip_image" != 1 ]; then
	command -v docker >/dev/null || fail "docker is needed for the image (or SKIP_IMAGE=1 to leave it out)"
fi
commit=$(git rev-parse HEAD)

step "make all VERSION=$version"
"$make" --no-print-directory all VERSION="$version"
[ -f web/dist/index.html ] || fail "web/dist/index.html is missing after the build: the UI would not be embedded"

step "check the version in both binaries"
for b in bin/kubemap-portal bin/kubemap-agent; do
	got=$("$b" version)
	[ "$got" = "$version" ] || fail "$b reports version $got, want $version"
done

step "smoke test"
scripts/smoke.sh bin/kubemap-portal bin/kubemap-agent "$version"

if [ "$skip_image" != 1 ]; then
	step "image $image:$version"
	"$make" --no-print-directory image VERSION="$version" IMAGE="$image"
	label=$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.version"}}' "$image:$version")
	[ "$label" = "$version" ] || fail "image label org.opencontainers.image.version is '$label', want $version"
	user=$(docker image inspect -f '{{.Config.User}}' "$image:$version")
	[ "$user" = "65532:65532" ] || fail "image user is '$user', want 65532:65532"
	got=$(docker run --rm --entrypoint /usr/local/bin/kubemap-portal "$image:$version" version)
	[ "$got" = "$version" ] || fail "the server in the image reports version $got, want $version"
	got=$(docker run --rm --entrypoint /usr/local/bin/kubemap-agent "$image:$version" version)
	[ "$got" = "$version" ] || fail "the node agent in the image reports version $got, want $version"
fi

# The build must not have changed tracked files (a lock file, generated code).
[ -z "$(git status --porcelain)" ] || fail "the build changed tracked files (git status); the release would not be reproducible"

step "write $dist/"
rm -rf "$dist"
mkdir -p "$dist"
bin/kubemap-agent bundle --image "$image:$version" >"$dist/kubemap-$version.yaml"
{
	echo "version: $version"
	echo "commit:  $commit"
	echo "go:      $(${GO:-go} env GOVERSION)"
	echo "manifest: kubemap-$version.yaml"
} >"$dist/RELEASE.txt"
if [ "$skip_image" != 1 ]; then
	id=$(docker image inspect -f '{{.Id}}' "$image:$version")
	printf '%s:%s\n%s\n' "$image" "$version" "$id" >"$dist/IMAGE.txt"
	docker save "$image:$version" | gzip -n >"$dist/kubemap-$version-image.tar.gz"
	echo "image:   $image:$version ($id)" >>"$dist/RELEASE.txt"
else
	echo "image:   not built (SKIP_IMAGE=1); this is not a complete release" >>"$dist/RELEASE.txt"
fi
(cd "$dist" && sha256sum -- * >SHA256SUMS)

step "done"
cat "$dist/RELEASE.txt"
echo
cat "$dist/SHA256SUMS"
