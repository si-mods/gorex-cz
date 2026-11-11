#!/bin/sh
# Install czgorex on a CardputerZero: binary plus the launcher entry.
set -e
PREFIX="${PREFIX:-$HOME/.local}"
APP_DIR=/usr/share/APPLaunch

install -d "$PREFIX/bin"
install -m 0755 czgorex "$PREFIX/bin/czgorex"

sudo install -d "$APP_DIR/applications" "$APP_DIR/share/images"
# point the launcher entry at wherever the binary actually landed
sed "s#^Exec=.*#Exec=$PREFIX/bin/czgorex#" packaging/czgorex.desktop > /tmp/czgorex.desktop
sudo install -m 0644 /tmp/czgorex.desktop "$APP_DIR/applications/czgorex.desktop"
rm -f /tmp/czgorex.desktop
sudo install -m 0644 assets/icon-256.png "$APP_DIR/share/images/czgorex_256.png"

echo "installed: $PREFIX/bin/czgorex"
echo "launcher entry: $APP_DIR/applications/czgorex.desktop"
echo "open GoRex from the launcher, or run: $PREFIX/bin/czgorex"
