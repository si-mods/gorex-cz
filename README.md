# gorex-cardputer

A minimal [GoRex](https://github.com/si-mods/gorex) client for the
**M5Stack CardputerZero** panel. It has no compositor, no GPU and no cgo: it
paints straight into `/dev/fb0` (320×170 RGB565), reads the matrix keyboard
from evdev, and drives the local GoRex session server through the `gorex` CLI.

```
┌──────────────────────────────────────────────┐
│ GoRex 34af3ff3              4/50 13:37       │  status bar
├──────────────────────────────────────────────┤
│ 30                                           │
│ 31                                           │  8 lines of the session
│ …                                            │  (scrollback, offset shown)
├──────────────────────────────────────────────┤
│ > uname -sm                                  │  input line
├──────────────────────────────────────────────┤
│   ↑       ⇤       ↵       ⇩       ↓          │  key bar: keys 4..8
│   │       │       │       │       │          │  ticks point at the keys
└──────────────────────────────────────────────┘
```

## Key bar

The bottom bar follows CardputerZero's published key guideline
([cardputer.cc → documents → CP0 keys](https://cardputer.cc/#/documents/cp0-keys)):
five 22×22 hints whose centres sit on **x = 49, 105, 161, 217, 273** at
**y = 153**, aligned with number keys **4–8**, each with a short vertical tick
pointing at its key.

| Key | Hint | Action |
|---|---|---|
| 4 | ↑ | scroll back one line |
| 5 | ⇤ | page up (8 lines) |
| 6 | ↵ | send the input line |
| 7 | ⇩ | page down (8 lines) |
| 8 | ↓ | scroll forward one line |

While the input line is empty, `F` `X` `Z` `C` act as the arrow keys
(up / down / page up / page down), as the guideline recommends for
non-text states. Type a digit first and keys 4–8 insert digits instead.

| Key | Action |
|---|---|
| Enter | send the line (`gorex send --enter`) |
| Backspace | delete a character |
| ESC | quit and restore the previous framebuffer content |

## Hosts and sessions

**TAB** (or the on-screen `TAB:close`) opens the picker:

```
Hosts and sessions                    TAB:close
this machine  | bash 34af3f  /home/pi
test          | zsh 91c2ab  /srv/app
4/8 mv  6 ok  5 pane  7 sync  0 add
```

| Key | Action |
|---|---|
| 4 / 8 | move the selection in the focused column |
| 6 | hosts → load that host's sessions; sessions → attach |
| 5 | focus the host column and reload its sessions |
| 7 | reload the selected host's sessions |
| 0 | add a host: type `name address` and Enter |
| TAB / ESC | close the picker |

The left column always starts with **this machine** (its local session server),
followed by every entry of `~/.gorex/hosts.json` as `gorex hosts` shows them.
The right column lists that host's sessions from `gorex ls --json`
(program, id, directory, `*` when attached). Choosing a session switches what
the terminal view attaches to; commands for a remote host run as
`gorex -S <host> capture|send`, so nothing about the local server changes.

**Adding a host** writes `{id, name, address}` into `~/.gorex/hosts.json`
(mode 0600). The address is the secret a remote GoRex server prints with
`gorex remote address`; that server still has to approve this machine's key —
`gorex remote pair start`, then `gorex remote pending` / `remote approve KEY`
on its side. Until it does, the host's session list reports unreachable.

## Fonts

The UI uses the fonts the CardputerZero image ships, as its app guideline asks
(12px or larger, Regular weight or heavier):

| Use | Face | Path |
|---|---|---|
| Terminal, input line | JetBrains Mono 12px, else DejaVu Sans Mono | `/usr/share/fonts/truetype/jetbrains-mono/JetBrainsMono-Regular.ttf`, `~/.local/share/fonts/JetBrainsMono-Regular.ttf`, `/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf` |
| Status bar, picker, lists | DejaVu Sans 12px | `/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf` |
| CJK (中文 / 日本語 / 한국어) | Noto Sans CJK SC 12px | `/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc` |

Text is drawn with `golang.org/x/image/font/opentype` (no cgo). A rune at or
above U+2E80 switches to the CJK face, which is loaded the first time such a
rune is drawn — the 19 MB collection costs ~45 MB RSS once it is in use, so a
few hundred KB until then. On this 320×170 panel 12px gives 7 visible lines of
session text.

The terminal face is picked at startup: JetBrains Mono (the guideline's
terminal font; `sudo apt install fonts-jetbrains-mono` on an image without it),
then DejaVu Sans Mono, then the bitmap face with `-bitmap`.

Layout follows the face: JetBrains Mono at 12px has a 17px line box, so the
grid pitch is 13px and 7 rows fit; DejaVu Sans Mono's 15px box gives the same
7 rows at a 13px pitch.

## Requirements

- A CardputerZero (Raspberry Pi CM0, Debian 13 aarch64) with `/dev/fb0` and the
  `tca8418` keyboard — the defaults auto-detect the keyboard by name.
- A GoRex session server on the same device:
  `gorex -server` (it listens on `~/.gorex/server.sock`; set `GOREX_DIR` if you
  moved it).
- Go 1.24+ to build. Cross-compile from anywhere:

```sh
make            # GOOS=linux GOARCH=arm64 CGO_ENABLED=0 → ./czgorex
```

## Install

```sh
make install    # binary → ~/.local/bin/czgorex
sudo make install-app   # launcher entry + icon → /usr/share/APPLaunch
```

`install-app` puts `czgorex.desktop` under
`/usr/share/APPLaunch/applications/` and the 256×256 icon under
`/usr/share/APPLaunch/share/images/`, so **GoRex appears in the CardputerZero
launcher**; the launcher yields the panel while the app runs and repaints when
it exits.

### Running it as a service instead

```ini
# ~/.config/systemd/user/czgorex.service
[Unit]
Conflicts=APPLaunch.service
[Service]
Environment=GOREX_DIR=%h/.gorex
ExecStartPre=-/usr/bin/systemctl --user stop APPLaunch.service
ExecStart=%h/.local/bin/czgorex
ExecStopPost=-/usr/bin/systemctl --user start APPLaunch.service
Restart=on-failure
[Install]
WantedBy=default.target
```

The launcher and this UI both own `/dev/fb0` and the same keyboard, so only one
of them may run: `Conflicts=` hands the panel over in both directions. A
`flock` on `/tmp/czgorex.lock` stops a second instance from grabbing it.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-fb` | `/dev/fb0` | framebuffer device |
| `-key` | auto | evdev device; empty auto-detects `tca8418` |
| `-gorex` | `~/.local/bin/gorex` | gorex executable |
| `-session` | first/created | session id to attach to |
| `-shot` | — | write a PNG + raw panel dump of every frame |
| `-secs` | 0 (forever) | exit after N seconds |
| `-fontsize` | 12 | text size in pixels (guideline: 12 or larger) |
| `-mono` | DejaVuSansMono.ttf | terminal font |
| `-ui-font` | DejaVuSans.ttf | status bar and picker font |
| `-cjk-font` | NotoSansCJK-Regular.ttc | CJK fallback |
| `-bitmap` | false | use the 7×13 bitmap face instead |
| `-overlay` | false | open the host picker at startup (for screenshots) |
| `-bench` | 0 | render N frames, print the average, exit |

Exiting (ESC, SIGTERM) writes the framebuffer content captured at startup back
to the panel, so the launcher's screen is restored rather than left half drawn.

## How the pieces fit

- **Display** — `openFB` mmaps `stride × 170` bytes (stride read from
  `/sys/class/graphics/fb0/stride`, 640 on this panel); `blit` converts an
  RGBA frame to RGB565 and writes it. A full frame costs ~5 ms on the CM0.
- **Input** — `readKeys` reads 24-byte `input_event` records from evdev and maps
  `KEY_*` codes to runes (`keymap.go`), tracking Shift.
- **Session** — `client` shells out to `gorex ls / capture --history / send`,
  polling `capture` every 500 ms (~144 ms per call) and keeping the scrollback
  in memory for the scroll keys.

## Status

MVP. Known gaps: no colour (ANSI SGR is stripped), no cursor addressing, one
session only, plain 7×13 bitmap font, and the CLI round trip per poll is
heavier than a direct socket client would be.

## License

MIT.
