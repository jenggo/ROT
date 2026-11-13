# rot

A small text viewer for Linux, built with [MyGo](https://mygo.run). It opens files
read-only by default, with syntax highlighting, and lets you flip a single tab into
edit mode when you actually need to change something. Untitled notes persist across
restarts, so it doubles as a scratchpad.

## Features

- **Tabs in one window** — open files from the Open dialog, the file manager, or the
  command line. Opening a file that is already open focuses its tab.
- **Read-only by default** — every file opens view-only, with selection and copy.
  Edit mode is per tab (`File > Edit Mode`).
- **Syntax highlighting** — around 40 languages, detected from the file name, with
  twelve themes (Default, One Dark, One Light, GitHub Dark/Light, Dracula, Nord,
  Gruvbox Dark, Tokyo Night, Solarized Light/Dark, Monokai).
- **Monospaced typography** — pick any monospaced family installed on the system and
  a size from 12 to 28.
- **Word wrap** — wrapped to the window width, on by default.
- **Notes that survive restarts** — `File > New` opens an untitled note that is kept
  in the app data directory until you save it somewhere or discard it.
- **Session restore** — open tabs and the active tab come back on launch; paths that
  no longer exist (or are no longer text) are dropped and the session is rewritten.
- **Text-only by design** — files must decode as UTF-8 and contain no NUL bytes;
  anything else is refused with a notification. Encoding conversion is out of scope.
- **Single instance** — opening a file from the file manager adds a tab to the running
  window and raises it.
- **Explicit save** — nothing is written to a file until you press Ctrl+S. A tab with
  unsaved edits shows a dot and asks Save / Discard / Cancel when closed.
- **Safe saves** — writes go to a temp file that replaces the original, keeping its
  permissions. If another program changed the file meanwhile, rot asks before
  overwriting it (the file's checksum is compared, not its timestamp).
- **Watches open files** — a file changed elsewhere reloads in its tab when the tab has
  nothing unsaved; otherwise a bar offers to reload.
- **Frugal with large files** — an inactive tab larger than 8 MB gives its buffer back
  and is read again when you return to it.

## Keyboard

| Shortcut | Action |
|---|---|
| `Ctrl+N` | New note |
| `Ctrl+O` | Open files |
| `Ctrl+S` | Save (a note asks where) |
| `Ctrl+W` | Close tab |
| `Ctrl+E` | Toggle edit mode for the active tab |
| `Ctrl+Left`, `Ctrl+PageUp` | Previous tab |
| `Ctrl+Right`, `Ctrl+PageDown` | Next tab |
| `Ctrl+,` | Settings |

The mouse wheel over the tab strip pans it sideways; clicking a tab switches to it.
`Ctrl+Tab` is left alone because desktops (KDE) use it to switch windows.

## Where data lives

Everything is under `~/.config/rot/` on Linux:

| File | Contents |
|---|---|
| `settings.json` | font family, font size, theme, word wrap |
| `session.json` | open tabs and the active tab |
| `notes/` | scratch files for untitled notes |
| `recent-documents.json` | the file manager's recent list (written by MyGo) |

Session and settings are written atomically (temp file then rename).

## Build and run

Requirements: Go 1.27, and GTK 3 at runtime on Linux (MyGo draws with OpenGL but uses
GTK for the window, menus and dialogs).

```sh
go tool mygo dev      # run with live reload
go tool mygo build    # package into dist/linux-amd64 (tar.gz, .deb, .rpm, PKGBUILD)
```

`mygo.json` is the app manifest: name, identifier, version, icon and the text
extensions rot claims in the file manager. `icon.png` in the repository root is the
app icon.

## Not included

- Split panes, minimap, plugins, and encoding conversion beyond UTF-8.
- macOS and Windows packaging: the code is portable, but only Linux is verified.
