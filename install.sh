#!/bin/sh
# Installs the latest np release for this machine into ~/.local/bin (or
# $NP_INSTALL_DIR) and makes sure that directory is on PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/EmreErdogan/np/main/install.sh | sh
set -eu

REPO="EmreErdogan/np"
DIR="${NP_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "np: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "np: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# Resolve the latest tag from the redirect, no API call needed.
tag=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')
[ -n "$tag" ] || { echo "np: could not determine latest release" >&2; exit 1; }
ver=${tag#v}
asset="np_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "np: downloading $tag for $os/$arch"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
fi
[ "$want" = "$got" ] || { echo "np: checksum mismatch, aborting" >&2; exit 1; }

mkdir -p "$DIR"
tar -xzf "$tmp/$asset" -C "$tmp" np
mv "$tmp/np" "$DIR/np"
chmod +x "$DIR/np"
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$DIR/np" 2>/dev/null || true
echo "np: installed $tag to $DIR/np"

# Make sure $DIR is on PATH for future shells.
case ":$PATH:" in
  *":$DIR:"*) ;;
  *)
    line="export PATH=\"$DIR:\$PATH\""
    shell=$(basename "${SHELL:-sh}")
    case "$shell" in
      zsh) rc="$HOME/.zshrc" ;;
      bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
      fish) rc="$HOME/.config/fish/config.fish"; line="fish_add_path $DIR" ;;
      *) rc="$HOME/.profile" ;;
    esac
    mkdir -p "$(dirname "$rc")"
    if ! grep -qsF "$DIR" "$rc"; then
      printf '\n# np\n%s\n' "$line" >> "$rc"
      echo "np: added $DIR to PATH in $rc"
    fi
    echo "np: open a new terminal, or run:  $line"
    ;;
esac

cat <<MSG

Next:
  np status              # confirm tailscale is up
  np hub <machine>       # pick the machine to sync through
  np service install     # run the daemon at login
MSG
