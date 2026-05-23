#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${T4_APISERVER_VERSION:-v0.35.4}"
WORKDIR="${T4_APISERVER_WORKDIR:-$ROOT/.tmp/apiserver-$VERSION}"
BIN_DIR="$ROOT/bin/apiserver-tests/$VERSION"
T4_BIN="${T4_APISERVER_T4_BIN:-$ROOT/bin/t4-apiserver-test}"

mkdir -p "$WORKDIR" "$BIN_DIR"
go build -o "$T4_BIN" "$ROOT/cmd/t4"
export T4_APISERVER_T4_BIN="$T4_BIN"

if [[ ! -d "$WORKDIR/.git" ]]; then
  rm -rf "$WORKDIR"
  git clone --depth 1 --branch "$VERSION" https://github.com/kubernetes/apiserver "$WORKDIR"
else
  git -C "$WORKDIR" fetch --depth 1 origin "refs/tags/$VERSION:refs/tags/$VERSION"
  git -C "$WORKDIR" reset --hard "$VERSION"
  git -C "$WORKDIR" clean -xffd
fi

cd "$WORKDIR"

grep -rlF 'k8s.io/apiserver/pkg/storage/etcd3/testserver' pkg/ \
  | xargs sed -i.bak 's|k8s.io/apiserver/pkg/storage/etcd3/testserver|github.com/t4db/t4-apiserver-testserver|'
find pkg -name '*.bak' -delete

go mod edit -replace "github.com/t4db/t4-apiserver-testserver=$ROOT/tests/apiserver/testserver"
go mod tidy
go test -c -o "$BIN_DIR/" ./pkg/storage/etcd3/...

for testbin in "$BIN_DIR"/*.test; do
  "$testbin" -test.count 1 -test.parallel 1 -test.timeout "${T4_APISERVER_TIMEOUT:-15m}"
done
