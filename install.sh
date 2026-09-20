#!/bin/sh
set -eu

repo="chuntley/android-transfer-slop"
base="https://github.com/$repo/releases/latest/download"
os_name=$(uname -s)
arch=$(uname -m)

case "$os_name-$arch" in
  Darwin-arm64)  asset=android-transfer-slop-darwin-arm64 ;;
  Darwin-x86_64) asset=android-transfer-slop-darwin-amd64 ;;
  Linux-aarch64) asset=android-transfer-slop-linux-arm64 ;;
  Linux-x86_64)  asset=android-transfer-slop-linux-amd64 ;;
  *)
    echo "Unsupported platform: $os_name-$arch (supported: macOS and Linux on arm64 or x86_64)" >&2
    exit 1
    ;;
esac

target=${INSTALL_PATH:-"$HOME/.local/bin/android-transfer-slop"}
target_dir=$(dirname "$target")
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/android-transfer-slop.XXXXXX")
tmp_binary="$tmp_dir/$asset"
tmp_target="$target.tmp.$$"
cleanup() {
  rm -rf "$tmp_dir"
  rm -f "$tmp_target"
}
trap cleanup EXIT HUP INT TERM

curl --fail --location --proto '=https' --tlsv1.2 --silent --show-error \
  --output "$tmp_binary" "$base/$asset"
curl --fail --location --proto '=https' --tlsv1.2 --silent --show-error \
  --output "$tmp_dir/SHA256SUMS" "$base/SHA256SUMS"

expected=$(awk -v asset="$asset" '$2 == asset || $2 == "dist/" asset { print $1; exit }' "$tmp_dir/SHA256SUMS")
if [ -z "$expected" ]; then
  echo "Release checksum is missing for $asset" >&2
  exit 1
fi

if command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp_binary" | awk '{ print $1 }')
elif command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp_binary" | awk '{ print $1 }')
else
  echo "Need shasum or sha256sum to verify the release" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "Release checksum mismatch for $asset" >&2
  exit 1
fi

mkdir -p "$target_dir"
cp "$tmp_binary" "$tmp_target"
chmod 0755 "$tmp_target"
mv -f "$tmp_target" "$target"
printf 'Installed %s\n' "$target"
case ":${PATH:-}:" in
  *":$target_dir:"*) printf 'Run: android-transfer-slop -gui\n' ;;
  *) printf 'Add to PATH: export PATH="%s:$PATH"\n' "$target_dir" ;;
esac
