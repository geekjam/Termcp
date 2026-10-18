package directive

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestPayloadIsSlicedNotReparsed pins the rule the whole format depends on: the
// payload is a SLICE OF THE SOURCE, so quoting, escaping and `$()` reach the shell
// exactly as the caller wrote them.
//
// The case that makes it a hard requirement is `'a'"b"'c'`: three shell parts in
// one word. Re-joining the parsed words yields `abc`, which is a different command
// line. Slicing yields the original.
func TestPayloadIsSlicedNotReparsed(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "a pipeline with quotes and && survives byte for byte",
			cmd:  `run a1f3:1 ls -la | grep -c '\.go$' && echo "rc=$?"`,
			want: `ls -la | grep -c '\.go$' && echo "rc=$?"`,
		},
		{
			name: "adjacent quoted parts are not merged",
			cmd:  `run a1f3:1 'a'"b"'c'`,
			want: `'a'"b"'c'`,
		},
		{
			name: "an unquoted multi-word payload is not re-quoted",
			cmd:  `run a1f3:1 echo a b`,
			want: `echo a b`,
		},
		{
			name: "dollar constructs are passed through, not expanded",
			cmd:  `run a1f3:1 'echo $HOME'`,
			want: `'echo $HOME'`,
		},
		{
			name: "backslashes survive",
			cmd:  `run a1f3:1 echo a\ b`,
			want: `echo a\ b`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(tc.cmd)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(f.Statements) != 1 {
				t.Fatalf("statements = %d, want 1", len(f.Statements))
			}
			if got := f.Statements[0].Payload; got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyParseNeverEvaluate pins that a payload is not interpreted: the parser
// does not expand $HOME, $(), or backticks, because expansion is the shell's job
// and doing it here would break the zero-rewrite guarantee.
func TestOnlyParseNeverEvaluate(t *testing.T) {
	f, err := Parse(`run a1f3:1 'echo $HOME $(date) ` + "`id`" + `'`)
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	for _, want := range []string{"$HOME", "$(date)", "`id`"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %q lost %q: the parser must not evaluate", payload, want)
		}
	}
}

// TestHashIsNotATargetPrefix pins the correction that produced the current
// syntax. `#` starts a comment in the shell grammar, so `run #a1f3:1 'x'` parses
// to a single word. That must be an ERROR with a usage line, never a silent
// no-op or an attempt to "fix" it.
func TestHashIsNotATargetPrefix(t *testing.T) {
	_, err := Parse(`run #a1f3:1 'x'`)
	if err == nil {
		t.Fatal("`run #a1f3:1` must fail: '#' is a comment, so the target is gone")
	}
	var derr *Error
	if !asError(err, &derr) {
		t.Fatalf("error is %T, want *directive.Error", err)
	}
	if !strings.Contains(derr.Msg, "needs a target") {
		t.Errorf("message = %q, want it to say the target is missing", derr.Msg)
	}
	if derr.Usage == "" {
		t.Error("the failure must carry a usage line, so a caller learns the shape")
	}
}

// TestLocatorSpellings pins both accepted target spellings: the short form
// (which every reply emits) and the old termcp:// form (which the Web UI's copy
// button still produces). The `#` inside a word is not a comment.
func TestLocatorSpellings(t *testing.T) {
	for _, cmd := range []string{`run a1f3:1 'x'`, `run termcp://#a1f3:1 'x'`} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got := f.Statements[0].Target; got != strings.Fields(cmd)[1] {
			t.Errorf("%s: target = %q", cmd, got)
		}
	}
}

// TestBatchTargetIsOneWord pins that a comma-separated target is a single word:
// the shell does not split on commas, so a batch needs no new syntax.
func TestBatchTargetIsOneWord(t *testing.T) {
	f, err := Parse(`run a1f3:1,b7c2:1 'x'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Target; got != "a1f3:1,b7c2:1" {
		t.Errorf("target = %q, want the whole comma list as one word", got)
	}
}

// TestFlagsComeAfterTheTarget pins the positional rule: word 2 is the target, so
// a flag in second position is refused rather than read as a target.
func TestFlagsComeAfterTheTarget(t *testing.T) {
	f, err := Parse(`key a1f3:1 up --repeat 3`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if st.Target != "a1f3:1" {
		t.Errorf("target = %q", st.Target)
	}
	if len(st.Args) != 1 || st.Args[0] != "up" {
		t.Errorf("args = %v, want [up]", st.Args)
	}
	if st.Flags["repeat"] != "3" {
		t.Errorf("repeat = %q, want 3", st.Flags["repeat"])
	}

	if _, err := Parse(`key --repeat 3 a1f3:1 up`); err == nil {
		t.Fatal("a flag before the target must be refused: it would silently become the target")
	} else if !strings.Contains(err.Error(), "after the target") {
		t.Errorf("error = %v, want it to say flags come after the target", err)
	}
}

// TestArgvTakesTheRestOfTheStatement pins that --argv is a word list: the words
// after it belong to the program, so flags must come before it and a bare
// payload is refused rather than dropped.
func TestArgvTakesTheRestOfTheStatement(t *testing.T) {
	f, err := Parse(`pipe internal --name chat --argv sh -c 'echo hi'`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if got, want := st.Flags["argv"], `sh -c 'echo hi'`; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if got := st.Flags["name"]; got != "chat" {
		t.Errorf("name = %q", got)
	}
	if st.Payload != "" {
		t.Errorf("payload = %q; --argv consumed it", st.Payload)
	}

	if _, err := Parse(`pipe internal --argv echo hi --name chat`); err == nil {
		t.Fatal("a known flag after --argv must be refused, not passed to the program")
	} else if !strings.Contains(err.Error(), "flags come before --argv") {
		t.Errorf("error = %v", err)
	}

	if _, err := Parse(`pipe internal sleep 5`); err == nil {
		t.Fatal("a bare payload must be refused, not silently dropped")
	} else if !strings.Contains(err.Error(), "--argv") {
		t.Errorf("error = %v, want it to name --argv", err)
	}
}

// TestSplitArgvReadsWordsNotSpaces pins the --argv splitting rule: words are
// read with the shell's quoting rules, exactly as a terminal would read them.
// There is no compatibility rule for a whole-quoted value — quoting the whole
// argv makes it one word, because that is what the terminal does.
func TestSplitArgvReadsWordsNotSpaces(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantCmd string
		want    []string
	}{
		{"unquoted words", "echo hello", "echo", []string{"hello"}},
		{"a quoted argument keeps its space", `sh -c "echo hello"`, "sh", []string{"-c", "echo hello"}},
		{"a quoted script is one word", `sh -c 'cd /srv && make'`, "sh", []string{"-c", "cd /srv && make"}},
		{"the whole argv quoted is one word", `'go build ./...'`, "go build ./...", nil},
		{"a quoted program name stays one word", `'sleep 5'`, "sleep 5", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args, err := SplitArgv(tc.raw)
			if err != nil {
				t.Fatalf("SplitArgv(%q): %v", tc.raw, err)
			}
			if cmd != tc.wantCmd || !slices.Equal(args, tc.want) {
				t.Errorf("SplitArgv(%q) = %q %v, want %q %v", tc.raw, cmd, args, tc.wantCmd, tc.want)
			}
		})
	}
}

// TestSplitArgvRefusesACommandLine pins that shell syntax is an error rather
// than text handed to the program: --argv is an argv, not a command line.
func TestSplitArgvRefusesACommandLine(t *testing.T) {
	for _, raw := range []string{
		`echo hi; ls`,
		`echo hi | wc`,
		`echo hi > /tmp/x`,
		`FOO=1 echo hi`,
		`echo "unbalanced`,
	} {
		if _, _, err := SplitArgv(raw); err == nil {
			t.Errorf("SplitArgv(%q) accepted a command line", raw)
		}
	}
}

// TestTargetOptionalFlagForms pins `watch --off <rule>` and `watch --list`:
// the documented flag-only forms have no target, so the positional rule must
// not read `--off` as one.
func TestTargetOptionalFlagForms(t *testing.T) {
	f, err := Parse(`watch --off notif_1`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if st.Target != "" {
		t.Errorf("target = %q, want empty for --off", st.Target)
	}
	if st.Flags["off"] != "notif_1" {
		t.Errorf("off = %q", st.Flags["off"])
	}

	f, err = Parse(`watch --list`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Statements[0].Flags["list"]; !ok {
		t.Error("--list must be present")
	}

	f, err = Parse(`watch a1f3:1 --event output`)
	if err != nil {
		t.Fatal(err)
	}
	if f.Statements[0].Target != "a1f3:1" || f.Statements[0].Flags["event"] != "output" {
		t.Errorf("target/event = %q/%q", f.Statements[0].Target, f.Statements[0].Flags["event"])
	}
}

// TestFlagWithValue pins both spellings of a valued flag.
func TestFlagWithValue(t *testing.T) {
	f, err := Parse(`read a1f3:1 --cursor 1024 --stream o --raw`)
	if err != nil {
		t.Fatal(err)
	}
	flags := f.Statements[0].Flags
	if flags["cursor"] != "1024" {
		t.Errorf("cursor = %q", flags["cursor"])
	}
	if _, ok := flags["raw"]; !ok {
		t.Error("a valueless flag must still be present: 'was it set' and 'what to' are two questions")
	}
	if flags["stream"] != "o" {
		t.Errorf("stream = %q", flags["stream"])
	}

	f, err = Parse(`read a1f3:1 --cursor=2048`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Flags["cursor"]; got != "2048" {
		t.Errorf("--cursor=2048 parsed as %q", got)
	}
}

// TestUnknownDirectiveAndFlag pins the two failures learn_mode turns into a
// Usage line: a name that is not a directive, and a flag that is not that
// directive's.
func TestUnknownDirectiveAndFlag(t *testing.T) {
	if _, err := Parse(`raed a1f3:1 'x'`); err == nil {
		t.Fatal("an unknown directive must fail")
	} else if !strings.Contains(err.Error(), "unknown directive") {
		t.Errorf("error = %v", err)
	}

	if _, err := Parse(`read a1f3:1 --bogus 1`); err == nil {
		t.Fatal("an unknown flag must fail")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %v", err)
	}
}

// TestStatementsSplitOnSemicolonNotOnAnd pins the sequencing rule: `;` asks two
// questions and gets two slots, while `&&` stays inside one payload because it is
// one command whose result the caller wants.
func TestStatementsSplitOnSemicolonNotOnAnd(t *testing.T) {
	f, err := Parse(`run a1f3:1 'x'; run b7c2:1 'y'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 2 {
		t.Fatalf("statements = %d, want 2", len(f.Statements))
	}
	if f.Statements[0].Target != "a1f3:1" || f.Statements[1].Target != "b7c2:1" {
		t.Errorf("targets = %q, %q", f.Statements[0].Target, f.Statements[1].Target)
	}

	f, err = Parse(`run a1f3:1 make && echo ok`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("`&&` split into %d statements; it is one command line", len(f.Statements))
	}
	if got := f.Statements[0].Payload; got != "make && echo ok" {
		t.Errorf("payload = %q", got)
	}
}

// TestEmptyIsLegal pins that an empty cmd is the "where am I" call: legal and
// cheap, never an error.
func TestEmptyIsLegal(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\n"} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if !f.Empty {
			t.Errorf("%q parsed as non-empty", cmd)
		}
	}
}

// TestPathIsTheCommandEcho pins the Path header: the caller's own bytes for that
// statement, so a combined command can be reconciled against what came back.
func TestPathIsTheCommandEcho(t *testing.T) {
	cmd := `run a1f3:1 'test -f go.mod && go test ./... || echo no-module'`
	f, err := Parse(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Path; got != cmd {
		t.Errorf("Path = %q, want %q", got, cmd)
	}
}

// TestEveryDirectiveParsesItsOwnUsage keeps the table honest: every FORM a usage
// line documents must be a line this parser accepts. A usage that does not parse
// teaches a shape the tool cannot run — which is how `watch --off <rule>` stayed
// documented and unparseable until a live caller tried it.
func TestEveryDirectiveParsesItsOwnUsage(t *testing.T) {
	for _, op := range Ops() {
		for _, form := range usageAlternatives.Split(op.Usage, -1) {
			line := fillUsageStubs(stripOptionals(form))
			if line == "" || strings.Contains(line, "…") {
				continue // prose, not a template
			}
			// A documented form starts with the directive's own name; anything
			// else after `|` is an explanation (`payload: …`, `只打字，不回车`).
			if line != op.Name && !strings.HasPrefix(line, op.Name+" ") {
				continue
			}
			if _, err := Parse(line); err != nil {
				t.Errorf("%s: usage form %q parses as %q, which fails: %v",
					op.Name, strings.TrimSpace(form), line, err)
			}
		}
	}
}

// usageAlternatives splits a usage line into its documented forms. The spaces
// are load-bearing: `|` inside a placeholder (<handle|session>) is not an
// alternative separator.
var usageAlternatives = regexp.MustCompile(`\s+\|\s+`)

// stripOptionals removes every bracketed group, nested ones included. Optional
// parts may be omitted, so what remains must parse on its own.
func stripOptionals(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// fillUsageStubs replaces every <placeholder> with a dummy word and normalizes
// spaces, so the line is something the parser can read.
func fillUsageStubs(s string) string {
	for {
		start := strings.IndexByte(s, '<')
		if start < 0 {
			break
		}
		end := strings.IndexByte(s[start:], '>')
		if end < 0 {
			break
		}
		s = s[:start] + "x" + s[start+end+1:]
	}
	return strings.Join(strings.Fields(s), " ")
}

// asError is errors.As without the import, kept local so the test reads plainly.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
