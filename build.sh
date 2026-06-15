#!/usr/bin/env bash
# Build the websh binaries. websh links against libpam via cgo, so it needs the
# PAM development header. On this host the system header isn't installed, so we
# extract it locally (see .builddeps) and point cgo at it. The system gcc must
# be used (a conda gcc on PATH has the wrong sysroot and fails to link libpam).
set -euo pipefail
cd "$(dirname "$0")"

# Prefer a locally-installed Go SDK and pin GOTOOLCHAIN=local so the build never
# tries to download a toolchain from go.dev (the system go may be too old and
# ~/go is ephemeral here, so the go.mod-driven toolchain fetch kept re-running).
# Extract one with: tar -C /opt/tools -xzf /opt/tools/download/go1.26.3.linux-amd64.tar.gz
for godir in /opt/tools/go "$HOME/go-sdk/go"; do
  if [ -x "$godir/bin/go" ]; then export PATH="$godir/bin:$PATH"; break; fi
done
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export CGO_ENABLED=1
# Force the system gcc: a conda gcc (often exported as $CC) has the wrong
# sysroot/glibc and fails to link libpam. Override with WEBSH_CC if needed.
export CC="${WEBSH_CC:-/usr/bin/gcc}"

BUILDDEPS="$(pwd)/.builddeps"
if [ -f "$BUILDDEPS/extracted/usr/include/security/pam_appl.h" ]; then
  export CGO_CFLAGS="-D_GNU_SOURCE -I$BUILDDEPS/extracted/usr/include"
  export CGO_LDFLAGS="-L$BUILDDEPS/lib"
fi

mkdir -p bin
echo "building websh (cgo/pam)…"
go build -o bin/websh ./cmd/websh
echo "building websh-notify…"
CGO_ENABLED=0 go build -o bin/websh-notify ./cmd/websh-notify
echo "done -> bin/websh bin/websh-notify"
