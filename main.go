// rot is a lightweight read-only text viewer for Linux: tabs of syntax
// highlighted files, an optional per-tab edit mode with explicit save, and
// a session that restores the last open set.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

const (
	sessionFile  = "session.json"
	settingsFile = "settings.json"
	// notesDir holds untitled notes under PathUserData.
	notesDir = "notes"
)

//go:embed icon.png
var appIcon []byte

// settings are the view preferences kept across runs.
type settings struct {
	Font  string  `json:"font"`
	Theme string  `json:"theme"`
	Size  float32 `json:"size"`
	Wrap  bool    `json:"wrap"`
}

// defaultSettings is the system monospaced font at 16 DIPs, GitHub colors.
var defaultSettings = settings{Size: 16, Theme: "Default", Wrap: true}

// tab is one open file: its buffer and editor state live across frames.
// A note has no real path: its text lives in a scratch file until saved.
type tab struct {
	buf     *editor.Buffer
	state   *editor.State
	path    string
	scratch string
	title   string
	// hash is the file's content when it was loaded or saved, to notice
	// changes made by other programs.
	hash uint32
	// readOnly is the tab's mode, kept while the buffer is unloaded.
	readOnly bool
	// watch and cancel follow the file on disk; external says it changed
	// there while the tab had unsaved edits.
	watch    *mygo.FileWatcher
	cancel   context.CancelFunc
	external bool
}

// unloadBytes is how large an inactive tab's text grows before its buffer
// is dropped and read again when the tab is chosen.
var unloadBytes = 8 << 20

// unload drops a clean, inactive tab's buffer: the file is read again when
// the tab is chosen, which costs a moment but keeps memory down.
func (t *tab) unload() {
	if t.buf == nil || t.scratch != "" || t.buf.Modified() || t.buf.Len() < unloadBytes {
		return
	}
	t.readOnly = t.state.ReadOnly
	t.buf, t.state = nil, nil
}

// unsaved reports whether closing the tab would lose text: it is edited,
// or it is a note holding something.
func (t *tab) unsaved() bool {
	if t.state == nil {
		return false
	}
	return t.state.Buffer.Modified() || (t.scratch != "" && t.buf.Len() > 0)
}

// name is what the tab and the window title call the tab.
func (t *tab) name() string {
	if t.path != "" {
		return filepath.Base(t.path)
	}
	return t.title
}

// app is the whole application state.
type app struct {
	// tabScroll keeps the tab strip's horizontal place.
	tabScroll ui.ScrollState
	win       *mygo.Window
	// editModeItem is the File > Edit Mode checkbox, kept in step with
	// the active tab's mode.
	editModeItem *mygo.MenuItem
	// themeCache is the editor theme for themeKey, rebuilt when the
	// theme or the window's dark mode changes.
	themeCache *editor.Theme
	// dataDirOverride, set by tests, redirects the session and settings
	// files to a temp directory instead of PathUserData.
	dataDirOverride string
	themeKey        string
	settings        settings
	// families caches the system's monospaced families for the picker.
	families []string
	tabs     []*tab
	active   int
	// noteSeq numbers the notes made this run.
	noteSeq int
	// followActive scrolls the tab strip to the tab chosen this frame.
	followActive bool
	// settingsOpen shows the settings modal.
	settingsOpen bool
}

// sessionData is the on-disk session file.
type sessionData struct {
	Tabs   []sessionTab `json:"tabs"`
	Active int          `json:"active"`
}

// sessionTab is one open tab: a file's path, or a note's scratch file and
// title. A plain string is a file, as earlier sessions stored them.
type sessionTab struct {
	Path    string `json:"path,omitempty"`
	Scratch string `json:"scratch,omitempty"`
	Title   string `json:"title,omitempty"`
}

func (s *sessionTab) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &s.Path)
	}
	type plain sessionTab
	return json.Unmarshal(b, (*plain)(s))
}

// fontSizes are the sizes the settings offer, in DIPs.
var fontSizes = []float32{12, 13, 14, 15, 16, 18, 20, 22, 24, 28}

// themeNames are the syntax highlighting palettes the settings offer.
var themeNames = []string{
	"Default", "One Dark", "One Light", "GitHub Dark", "GitHub Light",
	"Dracula", "Nord", "Gruvbox Dark", "Tokyo Night", "Solarized Light",
	"Solarized Dark", "Monokai",
}

// palette is a syntax highlighting palette: the editor's base colors and
// the token colors it overrides.
type palette struct {
	tokens                               map[editor.TokenKind]ui.Color
	background, text, lineNumber, cursor ui.Color
	selection, activeLine                ui.Color
	italicComments                       bool
}

// tokenKinds is the order tokenColors takes its hex colors in.
var tokenKinds = []editor.TokenKind{
	editor.TokenKeyword, editor.TokenType, editor.TokenFunction, editor.TokenVariable,
	editor.TokenProperty, editor.TokenConstant, editor.TokenNumber, editor.TokenString,
	editor.TokenEscape, editor.TokenRegexp, editor.TokenComment, editor.TokenOperator,
	editor.TokenPunctuation, editor.TokenTag, editor.TokenAttribute, editor.TokenMeta,
	editor.TokenHeading, editor.TokenLink, editor.TokenCode, editor.TokenInserted,
	editor.TokenDeleted, editor.TokenInvalid,
}

// tokenColors maps hex colors, in tokenKinds order, to token kinds.
func tokenColors(hexes ...string) map[editor.TokenKind]ui.Color {
	m := make(map[editor.TokenKind]ui.Color, len(hexes))
	for i, h := range hexes {
		m[tokenKinds[i]] = ui.Hex(h)
	}
	return m
}

// palettes are the syntax highlighting palettes the View menu offers.
var palettes = map[string]palette{
	"One Dark": {
		background: ui.Hex("#282c34"), text: ui.Hex("#abb2bf"),
		lineNumber: ui.Hex("#5c6370"), cursor: ui.Hex("#528bff"),
		selection: ui.RGBA(97, 175, 239, 0.25), activeLine: ui.RGBA(255, 255, 255, 0.05),
		italicComments: true,
		tokens: tokenColors(
			"#c678dd", "#e5c07b", "#61afef", "#e06c75", "#e06c75", "#d19a66",
			"#d19a66", "#98c379", "#56b6c2", "#98c379", "#5c6370", "#56b6c2",
			"#abb2bf", "#e06c75", "#d19a66", "#c678dd", "#e06c75", "#61afef",
			"#98c379", "#98c379", "#e06c75", "#e06c75",
		),
	},
	"Solarized Light": {
		background: ui.Hex("#fdf6e3"), text: ui.Hex("#657b83"),
		lineNumber: ui.Hex("#93a1a1"), cursor: ui.Hex("#268bd2"),
		selection: ui.RGBA(38, 139, 210, 0.2), activeLine: ui.RGBA(0, 0, 0, 0.04),
		italicComments: true,
		tokens: tokenColors(
			"#859900", "#b58900", "#268bd2", "#268bd2", "#268bd2", "#cb4b16",
			"#2aa198", "#2aa198", "#cb4b16", "#2aa198", "#93a1a1", "#859900",
			"#657b83", "#268bd2", "#b58900", "#cb4b16", "#cb4b16", "#268bd2",
			"#2aa198", "#859900", "#dc322f", "#dc322f",
		),
	},
	"Monokai": {
		background: ui.Hex("#272822"), text: ui.Hex("#f8f8f2"),
		lineNumber: ui.Hex("#75715e"), cursor: ui.Hex("#f8f8f0"),
		selection: ui.RGBA(73, 72, 62, 0.9), activeLine: ui.RGBA(255, 255, 255, 0.05),
		tokens: tokenColors(
			"#f92672", "#66d9ef", "#a6e22e", "#f8f8f2", "#f8f8f2", "#ae81ff",
			"#ae81ff", "#e6db74", "#ae81ff", "#e6db74", "#75715e", "#f92672",
			"#f8f8f2", "#f92672", "#a6e22e", "#75715e", "#a6e22e", "#66d9ef",
			"#e6db74", "#a6e22e", "#f92672", "#f92672",
		),
	},
	"One Light": {
		background: ui.Hex("#fafafa"), text: ui.Hex("#383a42"),
		lineNumber: ui.Hex("#9d9d9f"), cursor: ui.Hex("#526fff"),
		selection: ui.RGBA(82, 111, 255, 0.2), activeLine: ui.RGBA(0, 0, 0, 0.04),
		italicComments: true,
		tokens: tokenColors(
			"#a626a4", "#c18401", "#4078f2", "#e45649", "#e45649", "#986801",
			"#986801", "#50a14f", "#0184bc", "#50a14f", "#a0a1a7", "#0184bc",
			"#383a42", "#e45649", "#986801", "#a626a4", "#e45649", "#4078f2",
			"#50a14f", "#50a14f", "#e45649", "#e45649",
		),
	},
	"GitHub Dark": {
		background: ui.Hex("#0d1117"), text: ui.Hex("#c9d1d9"),
		lineNumber: ui.Hex("#6e7681"), cursor: ui.Hex("#58a6ff"),
		selection: ui.RGBA(56, 139, 253, 0.3), activeLine: ui.RGBA(255, 255, 255, 0.04),
		tokens: tokenColors(
			"#ff7b72", "#ffa657", "#d2a8ff", "#ffa657", "#79c0ff", "#79c0ff",
			"#79c0ff", "#a5d6ff", "#79c0ff", "#a5d6ff", "#8b949e", "#ff7b72",
			"#c9d1d9", "#7ee787", "#79c0ff", "#ff7b72", "#ff7b72", "#a5d6ff",
			"#a5d6ff", "#7ee787", "#ff7b72", "#ff7b72",
		),
	},
	"GitHub Light": {
		background: ui.Hex("#ffffff"), text: ui.Hex("#24292f"),
		lineNumber: ui.Hex("#8c959f"), cursor: ui.Hex("#0969da"),
		selection: ui.RGBA(9, 105, 218, 0.2), activeLine: ui.RGBA(0, 0, 0, 0.03),
		tokens: tokenColors(
			"#cf222e", "#953800", "#8250df", "#953800", "#0550ae", "#0550ae",
			"#0550ae", "#0a3069", "#0550ae", "#0a3069", "#6e7781", "#cf222e",
			"#24292f", "#116329", "#0550ae", "#cf222e", "#cf222e", "#0a3069",
			"#0a3069", "#116329", "#cf222e", "#cf222e",
		),
	},
	"Dracula": {
		background: ui.Hex("#282a36"), text: ui.Hex("#f8f8f2"),
		lineNumber: ui.Hex("#6272a4"), cursor: ui.Hex("#f8f8f2"),
		selection: ui.RGBA(68, 71, 90, 0.9), activeLine: ui.RGBA(255, 255, 255, 0.05),
		italicComments: true,
		tokens: tokenColors(
			"#ff79c6", "#8be9fd", "#50fa7b", "#f8f8f2", "#f8f8f2", "#bd93f9",
			"#bd93f9", "#f1fa8c", "#ff79c6", "#f1fa8c", "#6272a4", "#ff79c6",
			"#f8f8f2", "#ff79c6", "#50fa7b", "#ff79c6", "#ff79c6", "#8be9fd",
			"#f1fa8c", "#50fa7b", "#ff5555", "#ff5555",
		),
	},
	"Nord": {
		background: ui.Hex("#2e3440"), text: ui.Hex("#d8dee9"),
		lineNumber: ui.Hex("#4c566a"), cursor: ui.Hex("#88c0d0"),
		selection: ui.RGBA(136, 192, 208, 0.25), activeLine: ui.RGBA(255, 255, 255, 0.04),
		tokens: tokenColors(
			"#81a1c1", "#8fbcbb", "#88c0d0", "#d8dee9", "#d8dee9", "#8fbcbb",
			"#b48ead", "#a3be8c", "#ebcb8b", "#a3be8c", "#616e88", "#81a1c1",
			"#eceff4", "#81a1c1", "#8fbcbb", "#5e81ac", "#8fbcbb", "#88c0d0",
			"#a3be8c", "#a3be8c", "#bf616a", "#bf616a",
		),
	},
	"Gruvbox Dark": {
		background: ui.Hex("#282828"), text: ui.Hex("#ebdbb2"),
		lineNumber: ui.Hex("#7c6f64"), cursor: ui.Hex("#ebdbb2"),
		selection: ui.RGBA(80, 73, 69, 0.9), activeLine: ui.RGBA(255, 255, 255, 0.05),
		tokens: tokenColors(
			"#fb4934", "#fabd2f", "#b8bb26", "#83a598", "#83a598", "#d3869b",
			"#d3869b", "#b8bb26", "#fe8019", "#b8bb26", "#928374", "#fe8019",
			"#ebdbb2", "#fb4934", "#fabd2f", "#fb4934", "#fb4934", "#83a598",
			"#b8bb26", "#b8bb26", "#fb4934", "#fb4934",
		),
	},
	"Tokyo Night": {
		background: ui.Hex("#1a1b26"), text: ui.Hex("#c0caf5"),
		lineNumber: ui.Hex("#3b4261"), cursor: ui.Hex("#c0caf5"),
		selection: ui.RGBA(40, 52, 87, 0.9), activeLine: ui.RGBA(255, 255, 255, 0.04),
		tokens: tokenColors(
			"#bb9af7", "#2ac3de", "#7aa2f7", "#c0caf5", "#73daca", "#ff9e64",
			"#ff9e64", "#9ece6a", "#89ddff", "#9ece6a", "#565f89", "#89ddff",
			"#c0caf5", "#f7768e", "#e0af68", "#bb9af7", "#7aa2f7", "#7dcfff",
			"#9ece6a", "#9ece6a", "#f7768e", "#f7768e",
		),
	},
	"Solarized Dark": {
		background: ui.Hex("#002b36"), text: ui.Hex("#839496"),
		lineNumber: ui.Hex("#586e75"), cursor: ui.Hex("#93a1a1"),
		selection: ui.RGBA(38, 139, 210, 0.25), activeLine: ui.RGBA(255, 255, 255, 0.04),
		italicComments: true,
		tokens: tokenColors(
			"#859900", "#b58900", "#268bd2", "#268bd2", "#268bd2", "#cb4b16",
			"#2aa198", "#2aa198", "#cb4b16", "#2aa198", "#586e75", "#859900",
			"#839496", "#268bd2", "#b58900", "#cb4b16", "#cb4b16", "#268bd2",
			"#2aa198", "#859900", "#dc322f", "#dc322f",
		),
	},
}

// buildEditorTheme returns the editor theme named name for the window
// theme t.
func buildEditorTheme(name string, t *ui.Theme) *editor.Theme {
	ct := editor.DefaultTheme(t)
	p, ok := palettes[name]
	if !ok {
		return ct
	}
	ct.Background = p.background
	ct.Text = p.text
	ct.LineNumber = p.lineNumber
	ct.Cursor = p.cursor
	ct.Selection = p.selection
	ct.ActiveLine = p.activeLine
	for k, c := range p.tokens {
		ct.Tokens[k].Color = c
	}
	if p.italicComments {
		ct.Tokens[editor.TokenComment].Italic = true
	}
	return ct
}

// editorTheme returns the cached editor theme for the window theme t.
func (a *app) editorTheme(t *ui.Theme) *editor.Theme {
	key := a.settings.Theme + fmt.Sprint(t.Dark)
	if a.themeCache == nil || a.themeKey != key {
		a.themeCache = buildEditorTheme(a.settings.Theme, t)
		a.themeKey = key
	}
	return a.themeCache
}

// openFromOther opens files another instance or the system passed: they
// may come before the window exists.
func (a *app) openFromOther(paths []string) {
	if a.win == nil || len(paths) == 0 {
		return
	}
	a.win.Update(func() {
		a.showRejected(a.openPaths(paths))
		a.win.Focus()
	})
}

// argPaths returns the regular files among args: relative ones resolved
// against wd, flags skipped, and file:// URLs decoded.
func argPaths(args []string, wd string) []string {
	var paths []string
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		p := a
		if u, err := url.Parse(a); err == nil && u.Scheme == "file" {
			p = u.Path
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(wd, p)
		}
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			paths = append(paths, p)
		}
	}
	return paths
}

// crcTable is the checksum of file contents: hardware accelerated and
// plenty for telling whether another program changed a file.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

func hashBytes(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// readTextFile reads path, accepting only UTF-8 text without NUL bytes,
// and returns its content and checksum.
func readTextFile(path string) (string, uint32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	if !utf8.Valid(data) {
		return "", 0, fmt.Errorf("%s is not valid UTF-8", path)
	}
	if slices.Contains(data, 0) {
		return "", 0, fmt.Errorf("%s is not text (contains NUL bytes)", path)
	}
	// The editor copies the text into its rope, so this string only has to
	// live until then: sharing the read bytes avoids a second full copy of
	// a large file, which would double the peak at open.
	return unsafe.String(unsafe.SliceData(data), len(data)), hashBytes(data), nil
}

// newTab builds a read-only editor tab for the given text.
func newTab(path, text string) *tab {
	buf := editor.NewBuffer(text)
	buf.SetLanguage(editor.LanguageFor(path))
	return &tab{
		path: path,
		buf:  buf,
		state: &editor.State{
			Buffer:          buf,
			ReadOnly:        true,
			LineNumbers:     editor.LineNumbersOn,
			NoScrollPastEnd: true,
		},
	}
}

// newNote opens an empty untitled note, editable at once, whose text lives
// in a scratch file until it is saved somewhere.
func (a *app) newNote() {
	name := fmt.Sprintf("note-%d.txt", time.Now().UnixNano())
	scratch, err := a.dataPath(filepath.Join(notesDir, name))
	if err != nil {
		log.Printf("create note: %v", err)
		return
	}
	a.noteSeq++
	t := newTab("", "")
	t.scratch = scratch
	t.title = fmt.Sprintf("Untitled %d", a.noteSeq)
	t.state.ReadOnly = false
	a.tabs = append(a.tabs, t)
	a.active = len(a.tabs) - 1
	if a.editModeItem != nil {
		a.editModeItem.SetChecked(true)
	}
	a.saveSession()
}

// writeNote saves a note's text to its scratch file.
func (a *app) writeNote(t *tab) error {
	if err := os.MkdirAll(filepath.Dir(t.scratch), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(t.scratch, []byte(t.buf.String()))
}

// openPaths opens each valid path as a tab, focusing existing tabs instead
// of duplicating them. It reports the invalid paths for a toast.
func (a *app) openPaths(paths []string) []string {
	var rejected []string
	for _, p := range paths {
		p = filepath.Clean(p)
		if i := a.indexOf(p); i >= 0 {
			a.active = i
			continue
		}
		text, hash, err := readTextFile(p)
		if err != nil {
			rejected = append(rejected, p)
			continue
		}
		t := newTab(p, text)
		t.hash = hash
		a.tabs = append(a.tabs, t)
		a.watch(t)
		a.active = len(a.tabs) - 1
	}
	a.saveSession()
	a.unloadInactive()
	return rejected
}

// indexOf returns the tab index of path, or -1.
func (a *app) indexOf(path string) int {
	for i, t := range a.tabs {
		if t.path == path {
			return i
		}
	}
	return -1
}

// ensureLoaded reads the tab's file again after its buffer was unloaded.
func (a *app) ensureLoaded(t *tab) {
	if t.buf != nil || t.path == "" {
		return
	}
	text, hash, err := readTextFile(t.path)
	if err != nil {
		return
	}
	nt := newTab(t.path, text)
	nt.state.ReadOnly = t.readOnly
	t.buf, t.state = nt.buf, nt.state
	t.hash = hash
}

// unloadInactive drops the buffers of the inactive tabs worth re-reading.
func (a *app) unloadInactive() {
	for i, t := range a.tabs {
		if i != a.active {
			t.unload()
		}
	}
}

// watch follows the tab's file on disk, reloading it when it changes
// there and nothing in the tab does, and noting the change otherwise.
func (a *app) watch(t *tab) {
	if t.path == "" || a.win == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w, err := mygo.WatchFiles(ctx, t.path, mygo.FileWatchOptions{})
	if err != nil {
		cancel()
		return
	}
	t.watch, t.cancel = w, cancel
	go func() {
		for {
			if _, err := w.Next(ctx); err != nil {
				return
			}
			a.win.Update(func() { a.fileChanged(t) })
		}
	}()
}

// stopWatch stops following a tab's file.
func (t *tab) stopWatch() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.watch != nil {
		_ = t.watch.Close()
		t.watch = nil
	}
}

// fileChanged handles a change to the tab's file on disk: a reload when
// the tab has nothing unsaved, else a prompt to do so.
func (a *app) fileChanged(t *tab) {
	if !slices.Contains(a.tabs, t) || !changedOnDisk(t) {
		return
	}
	if t.state == nil {
		return // unloaded: the next load reads the file anyway
	}
	if t.state.Buffer.Modified() {
		t.external = true
		return
	}
	a.reload(t)
}

// reload reads the tab's file again, dropping what the tab showed.
func (a *app) reload(t *tab) {
	if t.buf == nil {
		return
	}
	text, hash, err := readTextFile(t.path)
	if err != nil {
		return
	}
	t.buf.Reset(text)
	t.hash = hash
	t.external = false
}

// closeTab removes tab i and selects a neighboring tab. It reports the
// closed tab so the caller can confirm a dirty tab first.
func (a *app) closeTab(i int) {
	t := a.tabs[i]
	t.stopWatch()
	if t.scratch != "" {
		_ = os.Remove(t.scratch)
	}
	a.tabs = append(a.tabs[:i], a.tabs[i+1:]...)
	switch {
	case len(a.tabs) == 0:
		a.active = 0
	case i < a.active:
		a.active--
	case a.active >= len(a.tabs):
		a.active = len(a.tabs) - 1
	}
	a.saveSession()
	a.unloadInactive()
}

// errSaveCanceled reports a save the user called off after the file was
// found changed on disk.
var errSaveCanceled = errors.New("save canceled")

// changedOnDisk reports whether the file's content differs from what the
// tab loaded or last saved.
func changedOnDisk(t *tab) bool {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return false
	}
	return hashBytes(data) != t.hash
}

// saveTab writes the tab's buffer to its file and clears the dirty state.
// A file another program changed is not overwritten without asking.
func (a *app) saveTab(t *tab) error {
	if changedOnDisk(t) {
		res, err := mygo.Dialog.Message(mygo.MessageOptions{
			Parent:  a.win,
			Type:    mygo.MessageWarning,
			Title:   "File changed on disk",
			Message: filepath.Base(t.path) + " changed since it was opened.",
			Detail:  "Saving now replaces those changes.",
			Buttons: []string{"Overwrite", "Cancel"},
		})
		if err != nil || res.Button != 0 {
			return errSaveCanceled
		}
	}
	blob := []byte(t.buf.String())
	if err := writeFileAtomic(t.path, blob); err != nil {
		return err
	}
	t.hash = hashBytes(blob)
	t.buf.MarkSaved()
	return nil
}

// dataPath returns the path of a file under PathUserData, or under the
// test override.
func (a *app) dataPath(name string) (string, error) {
	if a.dataDirOverride != "" {
		return filepath.Join(a.dataDirOverride, name), nil
	}
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// writeFileAtomic writes blob to path through a temp file and a rename, so
// a partial write never replaces a valid file. An existing file keeps its
// permissions.
func writeFileAtomic(path string, blob []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// saveSession writes the session file atomically.
func (a *app) saveSession() {
	path, err := a.dataPath(sessionFile)
	if err != nil {
		return
	}
	data := sessionData{Active: a.active}
	for _, t := range a.tabs {
		if t.scratch != "" {
			if err := a.writeNote(t); err != nil {
				log.Printf("save note %s: %v", t.title, err)
			}
			data.Tabs = append(data.Tabs, sessionTab{Scratch: t.scratch, Title: t.title})
			continue
		}
		data.Tabs = append(data.Tabs, sessionTab{Path: t.path})
	}
	blob, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, blob)
}

// saveSettings writes the settings file atomically.
func (a *app) saveSettings() {
	path, err := a.dataPath(settingsFile)
	if err != nil {
		return
	}
	blob, err := json.Marshal(a.settings)
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, blob)
}

// loadSettings reads the settings file, keeping the defaults for anything
// missing or unreadable.
func (a *app) loadSettings() {
	a.settings = defaultSettings
	path, err := a.dataPath(settingsFile)
	if err != nil {
		return
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var s struct {
		Font  string  `json:"font"`
		Theme string  `json:"theme"`
		Size  float32 `json:"size"`
		Wrap  *bool   `json:"wrap"`
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return
	}
	if s.Size > 0 {
		a.settings.Size = s.Size
	}
	if s.Theme != "" {
		a.settings.Theme = s.Theme
	}
	if s.Wrap != nil {
		a.settings.Wrap = *s.Wrap
	}
	a.settings.Font = s.Font
}

// loadSession reads the session file, dropping missing or invalid paths.
// It rewrites the file when any entry was pruned.
func (a *app) loadSession() {
	path, err := a.dataPath(sessionFile)
	if err != nil {
		return
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data sessionData
	if err := json.Unmarshal(blob, &data); err != nil {
		return
	}
	pruned := false
	for _, st := range data.Tabs {
		ok := a.loadNote(st)
		if st.Scratch == "" {
			ok = a.loadFileTab(st)
		}
		if !ok {
			pruned = true
		}
	}
	a.active = data.Active
	if a.active >= len(a.tabs) || a.active < 0 {
		a.active = 0
	}
	if pruned {
		a.saveSession()
	}
}

// loadNote restores an untitled note; false when its scratch file is gone.
func (a *app) loadNote(st sessionTab) bool {
	if st.Scratch == "" {
		return false
	}
	text, _, err := readTextFile(st.Scratch)
	if err != nil {
		return false
	}
	t := newTab("", text)
	t.scratch = st.Scratch
	t.title = st.Title
	if t.title == "" {
		t.title = "Untitled"
	}
	t.state.ReadOnly = false
	a.noteSeq++
	a.tabs = append(a.tabs, t)
	return true
}

// loadFileTab restores a file tab; false when the file is gone or is no
// longer text.
func (a *app) loadFileTab(st sessionTab) bool {
	if _, err := os.Stat(st.Path); err != nil {
		return false
	}
	text, hash, err := readTextFile(st.Path)
	if err != nil {
		return false
	}
	t := newTab(st.Path, text)
	t.hash = hash
	a.tabs = append(a.tabs, t)
	a.watch(t)
	return true
}

// showRejected toasts the paths that failed validation.
func (a *app) showRejected(paths []string) {
	for _, p := range paths {
		n := mygo.NewNotification(mygo.NotificationOptions{
			Title: "Cannot open file",
			Body:  p + " is not UTF-8 text",
		})
		if err := n.Show(); err != nil {
			log.Printf("show notification for %s: %v", p, err)
		}
	}
}

// openDialog runs the Open dialog off the main thread and applies the
// result with win.Update.
func (a *app) openDialog() {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:   a.win,
			Title:    "Open files",
			Multiple: true,
			Filters: []mygo.FileFilter{
				{Name: "Text files", MIMETypes: []string{"text/*"}},
				{Name: "All files", Extensions: []string{"*"}},
			},
		})
		if err != nil || len(paths) == 0 {
			return
		}
		a.win.Update(func() {
			a.showRejected(a.openPaths(paths))
			for _, p := range paths {
				_ = mygo.App.AddRecentDocument(p)
			}
		})
	}()
}

// confirmDirty asks Save / Discard / Cancel for a tab with unsaved edits.
// It returns 0 save, 1 discard, 2 cancel.
func (a *app) confirmDirty(t *tab) int {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Parent:  a.win,
		Type:    mygo.MessageQuestion,
		Title:   "Unsaved changes",
		Message: t.name() + " has unsaved changes.",
		Buttons: []string{"Save", "Discard", "Cancel"},
	})
	if err != nil {
		return 2
	}
	return res.Button
}

// requestClose closes tab i, asking about unsaved edits first.
func (a *app) requestClose(i int) {
	t := a.tabs[i]
	if t.unsaved() {
		switch a.confirmDirty(t) {
		case 0:
			if t.scratch != "" {
				a.saveNote(t, func() { a.closeTab(i) })
				return
			}
			if err := a.saveTab(t); err != nil {
				if !errors.Is(err, errSaveCanceled) {
					mygo.Dialog.Error("Save failed", err.Error())
				}
				return
			}
		case 2:
			return
		}
	}
	a.closeTab(i)
}

// closeActiveTab is the menu action for Close Tab.
func (a *app) closeActiveTab() {
	if a.active < len(a.tabs) {
		a.requestClose(a.active)
	}
}

// saveActiveTab is the menu action for Save: a note asks where to go.
func (a *app) saveActiveTab() {
	if a.active >= len(a.tabs) {
		return
	}
	t := a.tabs[a.active]
	if t.scratch != "" {
		a.saveNote(t, nil)
		return
	}
	if t.buf == nil {
		return
	}
	if err := a.saveTab(a.tabs[a.active]); err != nil && !errors.Is(err, errSaveCanceled) {
		mygo.Dialog.Error("Save failed", err.Error())
	}
}

// saveNote asks where note t goes, writes it there, drops its scratch
// file, and makes it a file tab. done runs after a successful save.
func (a *app) saveNote(t *tab, done func()) {
	go func() {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Parent:      a.win,
			Title:       "Save note",
			DefaultPath: t.title + ".txt",
		})
		if err != nil || path == "" {
			return
		}
		a.win.Update(func() {
			blob := []byte(t.buf.String())
			if err := writeFileAtomic(path, blob); err != nil {
				mygo.Dialog.Error("Save failed", err.Error())
				return
			}
			t.hash = hashBytes(blob)
			_ = os.Remove(t.scratch)
			t.scratch = ""
			t.path = path
			t.buf.SetLanguage(editor.LanguageFor(path))
			t.buf.MarkSaved()
			a.saveSession()
			if done != nil {
				done()
			}
		})
	}()
}

// menu builds the menu bar with the app's shortcuts and view options.
func (a *app) menu() *mygo.Menu {
	m := mygo.NewMenu([]*mygo.MenuItem{
		{Label: "File", Submenu: []*mygo.MenuItem{
			{Label: "New", Accelerator: "CmdOrCtrl+N", Click: func(*mygo.MenuItem, *mygo.Window) { a.newNote() }},
			{Label: "Open…", Accelerator: "CmdOrCtrl+O", Click: func(*mygo.MenuItem, *mygo.Window) { a.openDialog() }},
			{Label: "Save", Accelerator: "CmdOrCtrl+S", Click: func(*mygo.MenuItem, *mygo.Window) { a.saveActiveTab() }},
			{Type: mygo.MenuItemSeparator},
			{ID: "edit-mode", Type: mygo.MenuItemCheckbox, Label: "Edit Mode", Accelerator: "CmdOrCtrl+E",
				Click: func(*mygo.MenuItem, *mygo.Window) { a.toggleEditMode() }},
			{Type: mygo.MenuItemSeparator},
			{Label: "Close Tab", Accelerator: "CmdOrCtrl+W", Click: func(*mygo.MenuItem, *mygo.Window) { a.closeActiveTab() }},
			mygo.Separator(),
			{Role: mygo.RoleQuit},
		}},
		{Label: "View", Submenu: []*mygo.MenuItem{
			{ID: "word-wrap", Type: mygo.MenuItemCheckbox, Label: "Word Wrap", Checked: a.settings.Wrap,
				Click: func(*mygo.MenuItem, *mygo.Window) { a.settings.Wrap = !a.settings.Wrap; a.saveSettings() }},
			{Type: mygo.MenuItemSeparator},
			{Label: "Settings…", Accelerator: "CmdOrCtrl+,", Click: func(*mygo.MenuItem, *mygo.Window) { a.settingsOpen = true }},
		}},
	})
	a.editModeItem = m.ItemByID("edit-mode")
	return m
}

// systemFontLabel is the picker's entry for the system's monospaced font.
const systemFontLabel = "System Monospace"

// monoFamilies lists the system's monospaced families, sorted, for the
// font picker.
func (a *app) monoFamilies() []string {
	if a.families == nil {
		for _, f := range ui.FontFamilies() {
			if f.Monospaced {
				a.families = append(a.families, f.Name)
			}
		}
		sort.Strings(a.families)
	}
	return a.families
}

// settingsModal shows the font, size and theme pickers.
func (a *app) settingsModal(c *ui.Context) {
	t := c.Theme()
	ui.Modal(c, &a.settingsOpen, func() {
		ui.Column(c).Gap(10).Padding(20).Width(380).Children(func() {
			ui.Text(c, "Settings").FontSize(16).Bold()
			a.fontPicker(c, t)
			a.sizePicker(c, t)
			a.themePicker(c, t)
		})
	})
}

// fontPicker picks the monospaced family.
func (a *app) fontPicker(c *ui.Context, t *ui.Theme) {
	ui.Text(c, "Font").FontSize(12).TextColor(t.TextMuted)
	font := a.settings.Font
	if font == "" {
		font = systemFontLabel
	}
	if !ui.Select(c, &font, append([]string{systemFontLabel}, a.monoFamilies()...)).Changed() {
		return
	}
	if font == systemFontLabel {
		font = ""
	}
	a.settings.Font = font
	a.saveSettings()
}

// sizePicker picks the font size.
func (a *app) sizePicker(c *ui.Context, t *ui.Theme) {
	ui.Text(c, "Font Size").FontSize(12).TextColor(t.TextMuted)
	size := fmt.Sprintf("%g", a.settings.Size)
	sizes := make([]string, len(fontSizes))
	for i, s := range fontSizes {
		sizes[i] = fmt.Sprintf("%g", s)
	}
	if !ui.Select(c, &size, sizes).Changed() {
		return
	}
	if v, err := strconv.ParseFloat(size, 32); err == nil {
		a.settings.Size = float32(v)
		a.saveSettings()
	}
}

// themePicker picks the syntax highlighting palette.
func (a *app) themePicker(c *ui.Context, t *ui.Theme) {
	ui.Text(c, "Theme").FontSize(12).TextColor(t.TextMuted)
	theme := a.settings.Theme
	if !ui.Select(c, &theme, themeNames).Changed() {
		return
	}
	a.settings.Theme = theme
	a.saveSettings()
}

// tabBar builds the tab strip in the style of desktop editors: a strip
// darker than the editor, the active tab filled with the editor's color
// and underlined with the accent, inactive ones muted, and a red dot on
// a dirty tab.
func (a *app) tabBar(c *ui.Context) {
	// The strip scrolls sideways, and the wheel scrolls it too, as
	// browsers' tab strips do.
	ui.ScrollHorizontal(c).TrackScroll(&a.tabScroll).Height(34).HandleInput(a.wheelTabs).Children(func() {
		tabs := ui.TabsBase(c, &a.active, len(a.tabs))
		if tabs.List.Changed() {
			a.saveSession()
		}
		tabs.List.Gap(0).Padding(0, 8).Background(c.Theme().Surface).Children(func() {
			for i, tb := range a.tabs {
				a.tab(c, tabs, i, tb)
			}
		})
		a.followActive = false
	})
}

// wheelTabs pans the tab strip sideways with the wheel, as Notepad Next
// does: down goes toward the later tabs, up back. The active tab stays.
func (a *app) wheelTabs(ev ui.InputEvent) bool {
	if ev.Kind != ui.InputScroll || ev.DY == 0 {
		return false
	}
	a.tabScroll.X += ev.DY
	return true
}

// tab builds tab i: the active one filled with the editor's color and
// underlined with the accent, a dirty one with a red dot.
func (a *app) tab(c *ui.Context, tabs ui.TabsParts, i int, tb *tab) {
	t := c.Theme()
	tab := tabs.Tab(i).Padding(6, 12).Radius(0).Gap(6).FocusRing(false)
	on := i == a.active
	tab.TextColor(t.TextMuted)
	if on {
		tab.Background(t.Background).TextColor(t.Text).FontWeight(600)
	}
	if on && a.followActive {
		tab.ScrollIntoView()
	}
	tab.DrawOver(func(p *ui.Painter, r ui.Rect) {
		if on {
			p.Fill(ui.Rect{X: r.X, Y: r.Y + r.H - 2, W: r.W, H: 2}, t.Accent, 2)
		}
	})
	tab.Children(func() {
		if tb.state != nil && tb.state.Buffer.Modified() {
			ui.Text(c, "\u2022").TextColor(t.Danger)
		}
		ui.Text(c, tb.name()).SingleLine()
		ui.Icon(c, closeIcon).Size(12, 12).TextColor(t.TextMuted).OnClick(func() { a.requestClose(i) })
	})
}

// externalBar offers to reload a file that changed on disk while the tab
// had unsaved edits.
func (a *app) externalBar(c *ui.Context) {
	t := c.Theme()
	tb := a.tabs[a.active]
	ui.Row(c).Gap(12).Padding(6, 12).AlignItems(ui.Center).
		Background(t.Warning.Alpha(0.12)).Children(func() {
		ui.Text(c, tb.name()+" changed on disk").FontSize(12)
		ui.Box(c).Grow(1)
		ui.Button(c, "Reload").OnClick(func() { a.reload(tb) })
	})
}

// toggleEditMode flips the active tab between read-only and edit mode.
func (a *app) toggleEditMode() {
	if a.active < len(a.tabs) {
		tb := a.tabs[a.active]
		a.ensureLoaded(tb)
		if tb.state == nil {
			return
		}
		tb.state.ReadOnly = !tb.state.ReadOnly
		if a.editModeItem != nil {
			a.editModeItem.SetChecked(!tb.state.ReadOnly)
		}
	}
}

// view is the window content: tab bar, toolbar, editor or empty state.
func (a *app) view(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Fill().Children(func() {
		if a.active < len(a.tabs) {
			a.tabBar(c)
			ui.Row(c).Gap(12).Padding(4, 12).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
				st := a.tabs[a.active].state
				mode := "read-only"
				if !st.ReadOnly {
					mode = "edit"
				}
				ui.Text(c, mode).FontSize(12).TextColor(t.TextMuted)
				ui.Box(c).Grow(1)
				where := a.tabs[a.active].path
				if where == "" {
					where = "Not saved yet"
				}
				ui.Text(c, where).FontSize(12).TextColor(t.TextMuted)
			})
			a.ensureLoaded(a.tabs[a.active])
			a.syncTitle()
			if a.tabs[a.active].external {
				a.externalBar(c)
			}
			st := a.tabs[a.active].state
			st.Font = ui.Font{Family: a.settings.Font, Size: a.settings.Size}
			st.SoftWrap = a.settings.Wrap
			st.Theme = a.editorTheme(t)
			editor.View(c, st).Grow(1).FillWidth().AutoFocus()
		} else {
			a.syncTitle()
			ui.Box(c).Grow(1).Center().Children(func() {
				ui.Text(c, "Open a file to start (Ctrl+O)").TextColor(t.TextMuted)
			})
		}
		// Ctrl+Tab belongs to the desktop on KDE, so tabs switch on the
		// arrows and the page keys, as browsers' do.
		c.OnWindowShortcut(ui.Ctrl, ui.KeyRight, func() { a.switchTab(1) })
		c.OnWindowShortcut(ui.Ctrl, ui.KeyLeft, func() { a.switchTab(-1) })
		c.OnWindowShortcut(ui.Ctrl, ui.KeyPageDown, func() { a.switchTab(1) })
		c.OnWindowShortcut(ui.Ctrl, ui.KeyPageUp, func() { a.switchTab(-1) })
		a.settingsModal(c)
	})
}

// syncTitle keeps the window title in step with the active tab: the file
// name, a (write) suffix in edit mode, and the edited mark when dirty.
func (a *app) syncTitle() {
	if a.win == nil {
		return
	}
	title := "rot"
	edited := false
	if a.active < len(a.tabs) {
		tb := a.tabs[a.active]
		title = tb.name()
		if tb.state != nil && !tb.state.ReadOnly {
			title += " (write)"
		}
		if tb.state != nil {
			edited = tb.state.Buffer.Modified()
		}
	}
	a.win.SetTitle(title)
	a.win.SetDocumentEdited(edited)
}

// switchTab moves the active index by delta and wraps around.
func (a *app) switchTab(delta int) {
	n := len(a.tabs)
	if n == 0 {
		return
	}
	a.active = ((a.active+delta)%n + n) % n
	a.followActive = true
	tb := a.tabs[a.active]
	a.ensureLoaded(tb)
	if a.editModeItem != nil && tb.state != nil {
		a.editModeItem.SetChecked(!tb.state.ReadOnly)
	}
	a.saveSession()
	a.unloadInactive()
}

var closeIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>`))

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	a := &app{}
	mygo.App.OnOpenFile(func(path string) { a.openFromOther([]string{path}) })
	mygo.App.OnSecondInstance(func(args []string, workingDir string) {
		a.openFromOther(argPaths(args, workingDir))
	})
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) { a.saveSession() })

	startDir, err := os.Getwd()
	if err != nil {
		startDir = "."
	}
	mygo.App.WhenReady(func() {
		a.loadSettings()
		a.loadSession()
		// Files passed on the command line, whatever their extension:
		// mygo's own delivery only passes those of fileAssociations.
		a.showRejected(a.openPaths(argPaths(os.Args[1:], startDir)))
		// The app icon, for the title bar and the taskbar: on Linux this
		// is the default icon of every window.
		if err := mygo.App.Dock.SetIcon(appIcon); err != nil {
			log.Printf("set app icon: %v", err)
		}
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "rot",
			Width:   1000,
			Height:  700,
			Content: ui.View(a.view),
		})
		a.win.SetMenu(a.menu())
		// The session's tabs were restored before the window existed, so
		// their watches start now.
		for _, t := range a.tabs {
			a.watch(t)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
