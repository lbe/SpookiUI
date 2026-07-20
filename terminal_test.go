package main

import (
	"io"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// ---- Key parser (parseKey) ----

func TestParseKey(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantKind     keyKind
		wantRune     rune
		wantConsumed int
	}{
		// Incomplete input: parser asks for more bytes.
		{"empty", "", keyNone, 0, 0},
		{"bare esc incomplete", "\x1b", keyNone, 0, 0},
		{"esc bracket incomplete", "\x1b[", keyNone, 0, 0},
		{"esc ss3 incomplete", "\x1bO", keyNone, 0, 0},
		{"esc digit incomplete", "\x1b[3", keyNone, 0, 0},
		{"utf8 prefix incomplete", "\xc3", keyNone, 0, 0},

		// Arrows: CSI and SS3.
		{"up", "\x1b[A", keyUp, 0, 3},
		{"down", "\x1b[B", keyDown, 0, 3},
		{"right", "\x1b[C", keyRight, 0, 3},
		{"left", "\x1b[D", keyLeft, 0, 3},
		{"ss3 up", "\x1bOA", keyUp, 0, 3},
		{"ss3 down", "\x1bOB", keyDown, 0, 3},
		{"ss3 right", "\x1bOC", keyRight, 0, 3},
		{"ss3 left", "\x1bOD", keyLeft, 0, 3},

		// Home/End variants.
		{"home csi", "\x1b[H", keyHome, 0, 3},
		{"end csi", "\x1b[F", keyEnd, 0, 3},
		{"home ss3", "\x1bOH", keyHome, 0, 3},
		{"end ss3", "\x1bOF", keyEnd, 0, 3},
		{"home 1~", "\x1b[1~", keyHome, 0, 4},
		{"end 4~", "\x1b[4~", keyEnd, 0, 4},
		{"home 7~", "\x1b[7~", keyHome, 0, 4},
		{"end 8~", "\x1b[8~", keyEnd, 0, 4},

		// Delete, PgUp/PgDn, shift-tab.
		{"delete", "\x1b[3~", keyDelete, 0, 4},
		{"pgup", "\x1b[5~", keyPgUp, 0, 4},
		{"pgdn", "\x1b[6~", keyPgDn, 0, 4},
		{"shift-tab", "\x1b[Z", keyShiftTab, 0, 3},

		// Single-byte keys.
		{"enter cr", "\r", keyEnter, 0, 1},
		{"enter lf", "\n", keyEnter, 0, 1},
		{"backspace del", "\x7f", keyBackspace, 0, 1},
		{"backspace bs", "\x08", keyBackspace, 0, 1},
		{"tab", "\t", keyTab, 0, 1},
		{"ctrl-c", "\x03", keyCtrlC, 0, 1},
		{"ctrl-x", "\x18", keyCtrlX, 0, 1},

		// Printable ASCII.
		{"letter", "a", keyRune, 'a', 1},
		{"capital", "Z", keyRune, 'Z', 1},
		{"digit", "7", keyRune, '7', 1},
		{"space", " ", keyRune, ' ', 1},
		{"tilde", "~", keyRune, '~', 1},

		// UTF-8 runes above ASCII: decoded but flagged so text fields can drop them.
		{"utf8 2-byte", "é", keyUTF8, 'é', 2},
		{"utf8 3-byte", "中", keyUTF8, '中', 3},
		{"utf8 4-byte", "\U0001f47b", keyUTF8, '\U0001f47b', 4},

		// Unknown input is consumed gracefully.
		{"invalid utf8", "\xff", keyUnknown, 0, 1},
		{"ctrl-a", "\x01", keyUnknown, 0, 1},
		{"nul", "\x00", keyUnknown, 0, 1},
		{"esc + printable", "\x1bx", keyUnknown, 0, 2},
		{"unknown csi final", "\x1b[Q", keyUnknown, 0, 3},
		{"unknown ss3 final", "\x1bOQ", keyUnknown, 0, 3},
		{"unknown tilde seq", "\x1b[2~", keyUnknown, 0, 4},
		{"csi digit bad final", "\x1b[1;", keyUnknown, 0, 3},
	}
	for _, c := range cases {
		key, consumed := parseKey([]byte(c.in))
		if key.Kind != c.wantKind || key.Rune != c.wantRune || consumed != c.wantConsumed {
			t.Errorf("parseKey(%q) = (%v, %q, %d), want (%v, %q, %d)",
				c.in, key.Kind, key.Rune, consumed, c.wantKind, c.wantRune, c.wantConsumed)
		}
	}
}

func TestParseKeyChained(t *testing.T) {
	// A buffer holding several keys is parsed one event at a time.
	buf := []byte("a\x1b[Ab\x1b[3~")
	want := []KeyEvent{
		{Kind: keyRune, Rune: 'a'},
		{Kind: keyUp},
		{Kind: keyRune, Rune: 'b'},
		{Kind: keyDelete},
	}
	var got []KeyEvent
	for len(buf) > 0 {
		key, consumed := parseKey(buf)
		if key.Kind == keyNone || consumed == 0 {
			t.Fatalf("parseKey stalled on %q", buf)
		}
		got = append(got, key)
		buf = buf[consumed:]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chained parse = %v, want %v", got, want)
	}
}

// ---- Key reader goroutine ----

// fakeKeySrc feeds scripted chunks and scripted answers to the reader's
// "more bytes within the ESC timeout" hook.
type fakeKeySrc struct {
	chunks [][]byte
	waits  []bool
}

func (f *fakeKeySrc) Read(p []byte) (int, error) {
	if len(f.chunks) == 0 {
		return 0, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return copy(p, c), nil
}

func (f *fakeKeySrc) waitMore(time.Duration) bool {
	if len(f.waits) == 0 {
		return false
	}
	w := f.waits[0]
	f.waits = f.waits[1:]
	return w
}

func runKeyReader(src *fakeKeySrc) []KeyEvent {
	r := newKeyReader(src, src.waitMore)
	r.run() // synchronous: drains the fake until EOF, then closes the channel
	var got []KeyEvent
	for ev := range r.events {
		got = append(got, ev)
	}
	return got
}

func TestKeyReader(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		waits  []bool
		want   []KeyEvent
	}{
		{
			"sequence split across reads",
			[]string{"\x1b", "[A"}, []bool{true},
			[]KeyEvent{{Kind: keyUp}},
		},
		{
			"esc timeout yields bare escape",
			[]string{"\x1b"}, []bool{false},
			[]KeyEvent{{Kind: keyEscape}},
		},
		{
			"partial sequence timeout yields unknown",
			[]string{"\x1b", "["}, []bool{true, false},
			[]KeyEvent{{Kind: keyUnknown}},
		},
		{
			"plain text",
			[]string{"ab"}, nil,
			[]KeyEvent{{Kind: keyRune, Rune: 'a'}, {Kind: keyRune, Rune: 'b'}},
		},
		{
			"enter and ctrl-c",
			[]string{"\r\x03"}, nil,
			[]KeyEvent{{Kind: keyEnter}, {Kind: keyCtrlC}},
		},
		{
			"utf8 split across reads",
			[]string{"\xc3", "\xa9"}, []bool{true},
			[]KeyEvent{{Kind: keyUTF8, Rune: 'é'}},
		},
		{
			"multiple reads concatenated",
			[]string{"a", "\x1b[3~", "b"}, nil,
			[]KeyEvent{{Kind: keyRune, Rune: 'a'}, {Kind: keyDelete}, {Kind: keyRune, Rune: 'b'}},
		},
		{
			"eof with no input",
			nil, nil,
			nil,
		},
	}
	for _, c := range cases {
		var chunks [][]byte
		for _, s := range c.chunks {
			chunks = append(chunks, []byte(s))
		}
		src := &fakeKeySrc{chunks: chunks, waits: c.waits}
		if got := runKeyReader(src); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: events = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---- Termios raw-mode computation ----

func TestTermioctls(t *testing.T) {
	cases := []struct {
		goos     string
		get, set uintptr
	}{
		{"darwin", 0x40487413, 0x80487414}, // TIOCGETA/TIOCSETA
		{"linux", 0x5401, 0x5402},          // TCGETS/TCSETS
	}
	for _, c := range cases {
		get, set := termioctls(c.goos)
		if get != c.get || set != c.set {
			t.Errorf("termioctls(%q) = (%#x, %#x), want (%#x, %#x)",
				c.goos, get, set, c.get, c.set)
		}
	}
}

func TestMakeRawTermios(t *testing.T) {
	cases := []struct {
		goos        string
		vmin, vtime int
	}{
		{"darwin", 16, 17},
		{"linux", 6, 5},
	}
	for _, c := range cases {
		orig := syscall.Termios{
			Lflag: syscall.ICANON | syscall.ECHO | syscall.IEXTEN | syscall.ISIG,
			Iflag: syscall.ICRNL | syscall.IXON | syscall.ISTRIP | syscall.INPCK | syscall.IGNBRK,
		}
		orig.Cc[c.vmin] = 7
		orig.Cc[c.vtime] = 9
		raw := makeRawTermios(orig, c.goos)
		if raw.Lflag&(syscall.ICANON|syscall.ECHO|syscall.IEXTEN) != 0 {
			t.Errorf("%s: ICANON/ECHO/IEXTEN not cleared in Lflag %#x", c.goos, raw.Lflag)
		}
		if raw.Lflag&syscall.ISIG == 0 {
			t.Errorf("%s: ISIG must stay set (Ctrl-C -> SIGINT)", c.goos)
		}
		if raw.Iflag&(syscall.ICRNL|syscall.IXON|syscall.ISTRIP|syscall.INPCK) != 0 {
			t.Errorf("%s: ICRNL/IXON/ISTRIP/INPCK not cleared in Iflag %#x", c.goos, raw.Iflag)
		}
		if raw.Iflag&syscall.IGNBRK == 0 {
			t.Errorf("%s: unrelated input flags must be preserved", c.goos)
		}
		if raw.Cc[c.vmin] != 1 || raw.Cc[c.vtime] != 0 {
			t.Errorf("%s: VMIN/VTIME = %d/%d, want 1/0", c.goos, raw.Cc[c.vmin], raw.Cc[c.vtime])
		}
	}
}

// ---- Color capability probe ----

func TestSupports256Color(t *testing.T) {
	cases := []struct {
		term, colorTerm string
		want            bool
	}{
		{"xterm-256color", "", true},
		{"tmux-256color", "", true},
		{"ghostty", "", true},
		{"xterm-ghostty", "", true},
		{"xterm-kitty", "", true},
		{"xterm", "truecolor", true},
		{"xterm", "24bit", true},
		{"xterm", "", false},
		{"dumb", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := supports256Color(c.term, c.colorTerm); got != c.want {
			t.Errorf("supports256Color(%q, %q) = %v, want %v",
				c.term, c.colorTerm, got, c.want)
		}
	}
}

// ---- Fixed color-pair table (App._init_colors) ----

func TestFixedPairs(t *testing.T) {
	// fg/bg are curses color numbers; -1 is the terminal default.
	want := map[int][2]int{
		1:  {0, 6},  // black on cyan
		2:  {6, -1}, // cyan on default
		3:  {0, 7},  // black on white
		4:  {7, -1}, // white on default
		5:  {3, -1}, // yellow on default
		6:  {2, -1}, // green on default
		7:  {1, -1}, // red on default
		8:  {5, -1}, // magenta on default
		9:  {0, 3},  // black on yellow
		10: {5, -1}, // magenta on default
	}
	if !reflect.DeepEqual(fixedPairs, want) {
		t.Errorf("fixedPairs = %v, want %v", fixedPairs, want)
	}
}

func TestHexTo256(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"#ffffff", 231, true},
		{"#000000", 16, true},
		{"#ff0000", 196, true},
		{"#808080", 244, true},
		{"bogus", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := hexTo256(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("hexTo256(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ---- Frame buffer ----

func TestFrameBuffer(t *testing.T) {
	cases := []struct {
		name string
		rows int
		cols int
		ops  func(fb *frameBuffer)
		want string
	}{
		{
			"plain text", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{}) },
			"\x1b[1;1H\x1b[0mhi",
		},
		{
			"positioned", 24, 80,
			func(fb *frameBuffer) { fb.addstr(2, 4, "x", textAttr{}) },
			"\x1b[3;5H\x1b[0mx",
		},
		{
			"bold", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{bold: true}) },
			"\x1b[1;1H\x1b[0;1mhi",
		},
		{
			"reverse", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{reverse: true}) },
			"\x1b[1;1H\x1b[0;7mhi",
		},
		{
			"bold reverse", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{bold: true, reverse: true}) },
			"\x1b[1;1H\x1b[0;1;7mhi",
		},
		{
			"pair 1 black on cyan", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{pair: 1}) },
			"\x1b[1;1H\x1b[0;30;46mhi",
		},
		{
			"pair 2 cyan on default bg omitted", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{pair: 2}) },
			"\x1b[1;1H\x1b[0;36mhi",
		},
		{
			"pair 4 bold white", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{bold: true, pair: 4}) },
			"\x1b[1;1H\x1b[0;1;37mhi",
		},
		{
			"pair 9 black on yellow", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{pair: 9}) },
			"\x1b[1;1H\x1b[0;30;43mhi",
		},
		{
			"direct 256 fg", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{fg256: 196}) },
			"\x1b[1;1H\x1b[0;38;5;196mhi",
		},
		{
			"direct 256 bg", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{bg256: 21}) },
			"\x1b[1;1H\x1b[0;48;5;21mhi",
		},
		{
			"direct 256 fg and bg", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{fg256: 196, bg256: 21}) },
			"\x1b[1;1H\x1b[0;38;5;196;48;5;21mhi",
		},
		{
			"direct fg overrides pair fg, pair bg remains", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{pair: 1, fg256: 196}) },
			"\x1b[1;1H\x1b[0;38;5;196;46mhi",
		},
		{
			"clipped to width minus one column", 24, 10,
			func(fb *frameBuffer) { fb.addstr(0, 8, "abcd", textAttr{}) },
			"\x1b[1;9H\x1b[0ma",
		},
		{
			"last column emits nothing", 24, 10,
			func(fb *frameBuffer) { fb.addstr(0, 9, "ab", textAttr{}) },
			"",
		},
		{
			"row out of range", 24, 80,
			func(fb *frameBuffer) { fb.addstr(24, 0, "ab", textAttr{}) },
			"",
		},
		{
			"negative coords", 24, 80,
			func(fb *frameBuffer) {
				fb.addstr(-1, 0, "ab", textAttr{})
				fb.addstr(0, -1, "ab", textAttr{})
			},
			"",
		},
		{
			"clip counts runes not bytes", 24, 10,
			func(fb *frameBuffer) { fb.addstr(0, 5, "abédef", textAttr{}) },
			"\x1b[1;6H\x1b[0mabéd",
		},
		{
			"empty text emits nothing", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "", textAttr{bold: true}) },
			"",
		},
		{
			"visible cursor at edit point", 24, 80,
			func(fb *frameBuffer) {
				fb.addstr(0, 0, "prompt", textAttr{})
				fb.showCursor(0, 3)
			},
			"\x1b[1;1H\x1b[0mprompt\x1b[?25h\x1b[1;4H",
		},
		{
			"no cursor means no show sequence", 24, 80,
			func(fb *frameBuffer) { fb.addstr(0, 0, "hi", textAttr{}) },
			"\x1b[1;1H\x1b[0mhi",
		},
		{
			"writes accumulate in order", 24, 80,
			func(fb *frameBuffer) {
				fb.addstr(0, 0, "ab", textAttr{pair: 4})
				fb.addstr(1, 0, "cd", textAttr{})
			},
			"\x1b[1;1H\x1b[0;37mab\x1b[2;1H\x1b[0mcd",
		},
	}
	for _, c := range cases {
		fb := newFrameBuffer(c.rows, c.cols)
		c.ops(fb)
		if got := string(fb.render()); got != c.want {
			t.Errorf("%s: render() = %q, want %q", c.name, got, c.want)
		}
	}
}
