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

privileged=0
if [ -n "${INSTALL_PATH:-}" ]; then
  target=$INSTALL_PATH
  target_dir=$(dirname "$target")
  if [ -e "$target_dir" ] && { [ ! -d "$target_dir" ] || [ ! -w "$target_dir" ]; }; then
    privileged=1
  fi
else
  install_dir=""
  saved_ifs=$IFS
  IFS=:
  for candidate in ${PATH:-}; do
    IFS=$saved_ifs
    case "$candidate" in
      ""|.) continue ;;
      /*) ;;
      *) continue ;;
    esac
    if [ -d "$candidate" ] && [ -w "$candidate" ] && [ -x "$candidate" ]; then
      install_dir=$candidate
      break
    fi
    if [ ! -e "$candidate" ] && [ -w "$(dirname "$candidate")" ]; then
      install_dir=$candidate
      break
    fi
    IFS=:
  done
  IFS=$saved_ifs
  if [ -z "$install_dir" ]; then
    case ":${PATH:-}:" in
      *:/usr/local/bin:*)
        install_dir=/usr/local/bin
        privileged=1
        ;;
      *)
        echo "No writable directory on PATH. Add one or set INSTALL_PATH explicitly." >&2
        exit 1
        ;;
    esac
  fi
  target="$install_dir/android-transfer-slop"
  target_dir=$install_dir
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/android-transfer-slop.XXXXXX")
tmp_binary="$tmp_dir/$asset"
tmp_target="$tmp_dir/android-transfer-slop"
cleanup() {
  rm -rf "$tmp_dir"
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

if [ "$privileged" -eq 1 ]; then
  command -v sudo >/dev/null 2>&1 || {
    echo "The selected PATH directory is not writable and sudo is unavailable." >&2
    exit 1
  }
  sudo mkdir -p "$target_dir"
  sudo install -m 0755 "$tmp_binary" "$target"
else
  mkdir -p "$target_dir"
  cp "$tmp_binary" "$tmp_target"
  chmod 0755 "$tmp_target"
  mv -f "$tmp_target" "$target"
fi

printf 'Installed %s\n' "$target"
case ":${PATH:-}:" in
  *":$target_dir:"*) printf 'Run from anywhere: android-transfer-slop -gui\n' ;;
  *) printf 'Add to PATH: export PATH="%s:$PATH"\n' "$target_dir" ;;
esac
