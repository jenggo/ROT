package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTextFile(t *testing.T) {
	valid := writeFile(t, "ok.txt", "hello world\n")
	if _, _, err := readTextFile(valid); err != nil {
		t.Fatalf("valid UTF-8 rejected: %v", err)
	}

	bad := writeFile(t, "bad.txt", string([]byte{'h', 0xff, 0xfe}))
	if _, _, err := readTextFile(bad); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}

	nul := writeFile(t, "nul.bin", "a\x00b")
	if _, _, err := readTextFile(nul); err == nil {
		t.Fatal("NUL bytes accepted")
	}
}

func TestOpenPathsDedup(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	p1 := writeFile(t, "one.txt", "one")
	p2 := writeFile(t, "two.txt", "two")

	rejected := a.openPaths([]string{p1, p2})
	if len(rejected) != 0 || len(a.tabs) != 2 {
		t.Fatalf("open: rejected=%v tabs=%d", rejected, len(a.tabs))
	}

	a.openPaths([]string{p1})
	if len(a.tabs) != 2 {
		t.Fatalf("duplicate added: tabs=%d", len(a.tabs))
	}
	if a.active != 0 {
		t.Fatalf("existing tab not focused: active=%d", a.active)
	}
}

func TestCloseTabSelectsNeighbor(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	paths := []string{
		writeFile(t, "a.txt", "a"),
		writeFile(t, "b.txt", "b"),
		writeFile(t, "c.txt", "c"),
	}
	a.openPaths(paths)
	if a.active != 2 {
		t.Fatalf("active=%d, want last", a.active)
	}
	a.closeTab(2)
	if a.active != 1 || len(a.tabs) != 2 {
		t.Fatalf("after closing last: active=%d tabs=%d", a.active, len(a.tabs))
	}
	a.closeTab(0)
	if a.active != 0 || len(a.tabs) != 1 {
		t.Fatalf("after closing first: active=%d tabs=%d", a.active, len(a.tabs))
	}
	a.closeTab(0)
	if len(a.tabs) != 0 || a.active != 0 {
		t.Fatalf("after closing only tab: tabs=%d active=%d", len(a.tabs), a.active)
	}
}

func TestSaveTabWritesAndClearsDirty(t *testing.T) {
	path := writeFile(t, "s.txt", "before")
	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{path})
	tb := a.tabs[0]
	tb.buf.Edit(editor.Edit{Start: 0, End: 0, Text: "after "})
	if !tb.buf.Modified() {
		t.Fatal("buffer not dirty after edit")
	}
	if err := a.saveTab(tb); err != nil {
		t.Fatal(err)
	}
	if tb.buf.Modified() {
		t.Fatal("still dirty after save")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "after ") {
		t.Fatalf("file not saved: %q", got)
	}
}

func TestSessionPruneAndRestore(t *testing.T) {
	a := &app{}
	dir := t.TempDir()
	p1 := writeFile(t, "keep.txt", "keep")
	missing := filepath.Join(dir, "gone.txt")
	p3 := writeFile(t, "bad.txt", string([]byte{0xff}))

	a.dataDirOverride = dir
	sess, err := json.Marshal(sessionData{Tabs: []sessionTab{{Path: p1}, {Path: missing}, {Path: p3}}, Active: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), sess, 0o644); err != nil {
		t.Fatal(err)
	}
	a.loadSession()

	if len(a.tabs) != 1 || a.tabs[0].path != p1 {
		t.Fatalf("tabs=%v", a.tabs)
	}
	if a.active != 0 {
		t.Fatalf("active=%d", a.active)
	}
	// The session file must be rewritten without the stale entries.
	blob, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), missing) {
		t.Fatalf("session still lists missing path: %s", blob)
	}
}

func TestSaveTabKeepsModeAndHash(t *testing.T) {
	path := writeFile(t, "s.txt", "before")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{path})
	tb := a.tabs[0]
	tb.buf.Edit(editor.Edit{Start: 0, End: 0, Text: "after "})
	if err := a.saveTab(tb); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if tb.hash != hashBytes(data) {
		t.Fatal("hash not updated after save")
	}
	// A temp file must not be left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestChangedOnDisk(t *testing.T) {
	path := writeFile(t, "c.txt", "one")
	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{path})
	tb := a.tabs[0]
	if changedOnDisk(tb) {
		t.Fatal("fresh tab reported changed")
	}
	if err := os.WriteFile(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !changedOnDisk(tb) {
		t.Fatal("edited file not reported changed")
	}
}

func TestSessionAtomicWrite(t *testing.T) {
	a := &app{}
	dir := t.TempDir()
	a.dataDirOverride = dir
	p := writeFile(t, "x.txt", "x")
	a.openPaths([]string{p})

	tmp := filepath.Join(dir, sessionFile+".tmp")
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	blob, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), p) {
		t.Fatalf("session missing tab: %s", blob)
	}
}

func TestTabBarView(t *testing.T) {
	a := &app{}
	a.dataDirOverride = t.TempDir()
	a.openPaths([]string{
		writeFile(t, "a.go", "package main"),
		writeFile(t, "b.txt", "plain"),
	})
	tt := ui.NewTester(a.view, 800, 600)
	// The view must show one tab label per open file.
	if texts := tt.Texts(); len(texts) < 2 {
		t.Fatalf("expected 2 tab labels, got %d texts: %v", len(texts), texts)
	}
	if !tt.HasText("a.go") || !tt.HasText("b.txt") {
		t.Fatal("tab labels missing from view")
	}

	// A dirty tab shows the marker.
	a.tabs[1].buf.Edit(editor.Edit{Start: 0, End: 0, Text: "edit "})
	tt.Frame()
	if !tt.HasText("\u2022") {
		t.Fatal("dirty marker missing")
	}

	// Ctrl+PageDown cycles tabs; closing goes through the dirty confirm,
	// which needs a real backend dialog, so mark the tab saved first and
	// close through the same requestClose path.
	tt.Key(ui.Ctrl, ui.KeyPageDown)
	if a.active != 0 {
		t.Fatalf("Ctrl+PageDown did not wrap to tab 0: active=%d", a.active)
	}
	a.tabs[1].buf.MarkSaved()
	a.requestClose(1)
	a.requestClose(0)
	if len(a.tabs) != 0 {
		t.Fatalf("tabs left open: %d", len(a.tabs))
	}
	tt.Frame()
	if !tt.HasText("Open a file to start") {
		t.Fatal("empty state not shown after closing all tabs")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &app{dataDirOverride: dir}
	a.loadSettings()
	if a.settings != defaultSettings {
		t.Fatalf("defaults: %+v", a.settings)
	}

	a.settings = settings{Font: "Fira Code", Size: 16, Theme: "Monokai", Wrap: true}
	a.saveSettings()

	b := &app{dataDirOverride: dir}
	b.loadSettings()
	if b.settings != a.settings {
		t.Fatalf("loaded %+v, want %+v", b.settings, a.settings)
	}
}

func TestSettingsKeepWrapDefault(t *testing.T) {
	dir := t.TempDir()
	// A settings file from before wrap existed keeps the wrapped default.
	old := []byte(`{"font":"Fira Code","size":14,"theme":"Nord"}`)
	if err := os.WriteFile(filepath.Join(dir, settingsFile), old, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDirOverride: dir}
	a.loadSettings()
	if !a.settings.Wrap {
		t.Fatal("wrap default lost")
	}
	if a.settings.Size != 14 {
		t.Fatalf("size=%v", a.settings.Size)
	}

	// Turning it off persists.
	a.settings.Wrap = false
	a.saveSettings()
	b := &app{dataDirOverride: dir}
	b.loadSettings()
	if b.settings.Wrap {
		t.Fatal("wrap on after saving it off")
	}
}

func TestSettingsModal(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	a.settingsOpen = true
	tt := ui.NewTester(a.view, 800, 600)
	for _, want := range []string{"Settings", "Font", "Font Size", "Theme"} {
		if !tt.HasText(want) {
			t.Fatalf("settings modal missing %q", want)
		}
	}
}

func TestWheelPansTabs(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{
		writeFile(t, "a.txt", "a"),
		writeFile(t, "b.txt", "b"),
		writeFile(t, "c.txt", "c"),
	})
	a.active = 1

	if !a.wheelTabs(ui.InputEvent{Kind: ui.InputScroll, DY: 40}) {
		t.Fatal("wheel down not taken")
	}
	if a.tabScroll.X != 40 {
		t.Fatalf("X=%v, want 40", a.tabScroll.X)
	}
	if a.active != 1 {
		t.Fatalf("wheel changed the tab: active=%d", a.active)
	}
	if !a.wheelTabs(ui.InputEvent{Kind: ui.InputScroll, DY: -15, Precise: true}) {
		t.Fatal("touchpad scroll not taken")
	}
	if a.tabScroll.X != 25 {
		t.Fatalf("X=%v, want 25", a.tabScroll.X)
	}
	if a.wheelTabs(ui.InputEvent{Kind: ui.InputScroll}) {
		t.Fatal("wheel with no delta taken")
	}
}

func TestNotesPersist(t *testing.T) {
	dir := t.TempDir()
	a := &app{dataDirOverride: dir}
	a.newNote()
	if a.tabs[0].scratch == "" {
		t.Fatal("note has no scratch file")
	}
	if a.tabs[0].state.ReadOnly {
		t.Fatal("new note is read-only")
	}
	a.tabs[0].buf.Edit(editor.Edit{Start: 0, End: 0, Text: "remember this"})
	a.saveSession()

	b := &app{dataDirOverride: dir}
	b.loadSession()
	if len(b.tabs) != 1 {
		t.Fatalf("tabs=%d", len(b.tabs))
	}
	if got := b.tabs[0].buf.String(); got != "remember this" {
		t.Fatalf("note text %q", got)
	}
	if b.tabs[0].title != "Untitled 1" {
		t.Fatalf("title %q", b.tabs[0].title)
	}
}

func TestNoteWithTextIsUnsaved(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	a.newNote()
	if a.tabs[0].unsaved() {
		t.Fatal("empty note reported unsaved")
	}
	a.tabs[0].buf.Edit(editor.Edit{Start: 0, End: 0, Text: "hi"})
	if !a.tabs[0].unsaved() {
		t.Fatal("note with text reported saved")
	}
	a.tabs[0].buf.MarkSaved()
	if !a.tabs[0].unsaved() {
		t.Fatal("note with text reported saved after MarkSaved")
	}
}

func TestCloseNoteRemovesScratch(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	a.newNote()
	scratch := a.tabs[0].scratch
	a.closeTab(0)
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("scratch file left behind")
	}
}

func TestSessionStringTabsMigrate(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, "old.txt", "old")
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"tabs":["`+p+`"],"active":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDirOverride: dir}
	a.loadSession()
	if len(a.tabs) != 1 || a.tabs[0].path != p {
		t.Fatalf("tabs=%v", a.tabs)
	}
}

func TestArgPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(p, []byte("select 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := argPaths([]string{"--flag", "dump.sql", "missing.txt", "file://" + p}, dir)
	if len(got) != 2 || got[0] != p || got[1] != p {
		t.Fatalf("paths=%v, want %q twice", got, p)
	}
}

func TestArrowKeysSwitchTabs(t *testing.T) {
	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{writeFile(t, "a.txt", "a"), writeFile(t, "b.txt", "b")})
	a.active = 0
	tt := ui.NewTester(a.view, 400, 300)
	tt.Key(ui.Ctrl, ui.KeyRight)
	if a.active != 1 {
		t.Fatalf("Ctrl+Right: active=%d, want 1", a.active)
	}
	tt.Key(ui.Ctrl, ui.KeyLeft)
	if a.active != 0 {
		t.Fatalf("Ctrl+Left: active=%d, want 0", a.active)
	}
	tt.Key(ui.Ctrl, ui.KeyPageDown)
	if a.active != 1 {
		t.Fatalf("Ctrl+PageDown: active=%d, want 1", a.active)
	}
	tt.Key(ui.Ctrl, ui.KeyPageUp)
	if a.active != 0 {
		t.Fatalf("Ctrl+PageUp: active=%d, want 0", a.active)
	}
}

func TestUnloadInactiveTabs(t *testing.T) {
	old := unloadBytes
	unloadBytes = 4
	defer func() { unloadBytes = old }()

	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{
		writeFile(t, "a.txt", "aaaa"),
		writeFile(t, "b.txt", "bbbb"),
	})
	if a.tabs[0].buf != nil {
		t.Fatal("inactive large tab not unloaded")
	}
	if a.tabs[1].buf == nil {
		t.Fatal("active tab unloaded")
	}

	a.switchTab(-1)
	if a.tabs[0].buf == nil {
		t.Fatal("tab not loaded when chosen")
	}
	if got := a.tabs[0].buf.String(); got != "aaaa" {
		t.Fatalf("reloaded text %q", got)
	}
	if a.tabs[1].buf != nil {
		t.Fatal("previously active tab not unloaded")
	}
}

func TestUnsavedTabNotUnloaded(t *testing.T) {
	old := unloadBytes
	unloadBytes = 4
	defer func() { unloadBytes = old }()

	a := &app{dataDirOverride: t.TempDir()}
	a.openPaths([]string{
		writeFile(t, "a.txt", "aaaa"),
		writeFile(t, "b.txt", "bbbb"),
	})
	a.switchTab(-1) // loads tab 0
	a.tabs[0].buf.Edit(editor.Edit{Start: 0, End: 0, Text: "edited "})
	a.switchTab(1)
	if a.tabs[0].buf == nil {
		t.Fatal("tab with unsaved edits unloaded")
	}
}

func TestLanguageFor(t *testing.T) {
	if editor.LanguageFor("main.go") == nil {
		t.Fatal("no language for .go")
	}
	_ = editor.LanguageFor("unknown.xyz") // nil is fine: plain text
}
