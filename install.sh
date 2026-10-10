#!/bin/sh
set -eu

repo=Bhavya031/Glance
version=${1:-}
install_dir=${GLANCE_INSTALL_DIR:-"$HOME/.local/bin"}

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to install Glance." >&2
  exit 1
fi
if [ -z "$version" ]; then
  latest=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
  version=${latest##*/}
fi
case "$version" in
  v[0-9A-Za-z]* ) ;;
  *) echo "Invalid release version: $version" >&2; exit 1 ;;
esac
case "$version" in *[!0-9A-Za-z._+-]*) echo "Invalid release version: $version" >&2; exit 1 ;; esac

release=${version#v}
asset="Glance_${release}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t glance)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

curl -fL --retry 3 -o "$tmp/$asset" "$base/$asset"
curl -fL --retry 3 -o "$tmp/checksums.txt" "$base/checksums.txt"
expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$tmp/checksums.txt")
case "$expected" in ''|*[!0-9A-Fa-f]*) valid_checksum=false ;; *) valid_checksum=true ;; esac
if [ "$valid_checksum" != true ] || [ ${#expected} -ne 64 ]; then
  echo "Missing or invalid checksum for $asset." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
if [ "$(printf '%s' "$actual" | tr 'A-F' 'a-f')" != "$(printf '%s' "$expected" | tr 'A-F' 'a-f')" ]; then
  echo "Checksum verification failed for $asset." >&2
  exit 1
fi

tar -xzf "$tmp/$asset" -C "$tmp" glance
mkdir -p "$install_dir"
install -m 755 "$tmp/glance" "$install_dir/glance"
echo "Installed glance at $install_dir/glance"
case ":$PATH:" in
  *:"$install_dir":*) echo "glance is ready to use." ;;
  *) echo "Add $install_dir to PATH, then run glance." ;;
esac
