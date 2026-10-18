// Package directive turns the single-entry `term` tool's `cmd` string into
// statements: which directive, which target, which flags, and — the part that
// must not be re-derived anywhere — the payload, taken as a SLICE OF THE SOURCE
// rather than re-assembled from parsed words.
//
// It parses with the real shell grammar (mvdan.cc/sh/v3/syntax) and never
// evaluates: `$HOME` in a payload is not expanded here, it is handed to the
// shell that will expand it. The rule that makes the whole format work is the
// one in the issue that produced this package: every word in `cmd` must be a word
// the shell itself can parse. `#` is a comment character, so a locator can never
// start with one — `run #a1f3:1 'x'` parses to just `run`, and that is reported
// as an error rather than silently fixed up.
//
// Two shapes exist, and the difference is a property of the directive:
//
//	payload  (run, alias):        everything after the target is raw bytes
//	flags    (key, read, file, …): --name[=value] after the target
//
// `pipe`/`pty` are payload directives whose `--argv` flag takes the REST of the
// statement: an argv is a word list (`--argv sh -c 'echo hi'`), and reading only
// the word after the flag would silently drop the program's arguments.
//
// Flags go AFTER the target, never before: `key --repeat 3 a1f3:1 up` is
// rejected, because a flag in second position breaks the one positional rule the
// whole syntax rests on ("word 2 is the target").
package directive

import (
	"fmt"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Kind says how the words after the target are read.
type Kind int

const (
	// KindFlags takes --name[=value] options and positional arguments.
	KindFlags Kind = iota
	// KindPayload hands every byte after the target to the directive unchanged.
	// The bytes come from the source string, so quoting survives: `'a'"b"'c'`
	// reaches the shell as `'a'"b"'c'`, not as `abc`.
	KindPayload
	// KindInterpreted is a payload that is also a NAME, not a command line
	// (`alias jira mcp open <url>`): the payload is one shell word, so a
	// following argument is not part of it.
	KindInterpreted
)

// Op is one directive's grammar.
type Op struct {
	Name string
	// Kind selects how the words after the target are read.
	Kind Kind
	// Target says the second word names the object the directive acts on.
	Target bool
	// TargetOptional allows the flag-only forms of a targeted directive
	// (`watch --off <rule>`, `watch --list`), where there is no object to name.
	// Without it the positional rule ("word 2 is the target") would reject a
	// leading flag, so the documented forms could never parse.
	TargetOptional bool
	// Flags lists the accepted option names (without the leading --).
	Flags []string
	// MinArgs is the minimum number of POSITIONAL arguments after the target,
	// for the flag kinds (a payload kind is governed by whether a payload exists
	// at all). It is checked so a truncated command fails here, with a usage
	// line, rather than three layers down with "shell not found".
	MinArgs int
	// Valueless lists the flags that take no value, so the word after them is
	// not swallowed as one. Every other flag consumes the next word when it was
	// not written as --name=value.
	Valueless []string
	// RestValue lists the flags whose value is the REST of the statement, not
	// one word. An argv is a word list; stopping at the first word would drop
	// the program's arguments without an error, which is the worst failure
	// mode. These flags must come last: a known flag after one is refused.
	RestValue []string
	// Usage is the one-line template `help` and error replies print. Window
	// parameters are deliberately absent: they are on the tool schema with
	// self-explaining names, and repeating them 22 times would be noise.
	Usage string
	// Detail is the multi-line knowledge `help <directive>` adds on request: the
	// shapes and traps a caller needs to produce something the server accepts,
	// which do not fit a one-line usage template and would be noise in an error.
	Detail string
}

// Statement is one directive after parsing.
type Statement struct {
	Op     string
	Target string
	// Args are the positional arguments after the target.
	Args []string
	// Flags holds --name[=value] options. A flag given without a value is
	// present with an empty string, so "was it set" and "what was it set to" are
	// two questions with two answers.
	Flags map[string]string
	// Payload is the raw source slice handed to the directive, for the payload
	// kinds. It is EMPTY for a directive that takes none.
	Payload string
	// Path is the directive as it will be shown back in the reply's Path header:
	// the source slice for this statement, trimmed. It is the command echo, so it
	// is the caller's own bytes, not a canonical re-rendering.
	Path string
}

// Error is a directive-level failure: unknown op, bad flag, missing argument.
// It carries the usage line so the reply can teach, which is why it is a type
// and not a wrapped fmt.Errorf.
type Error struct {
	// Op is the directive the failure is about, or "" when the first word was
	// not a directive at all.
	Op string
	// Msg is what went wrong, in the caller's terms.
	Msg string
	// Usage is the one-line template for Op, when one is known.
	Usage string
	// Rest is the statement after Op, verbatim, so a caller-defined directive can
	// be re-parsed once its meaning is known.
	Rest string
}

func (e *Error) Error() string {
	if e.Usage != "" {
		return e.Msg + "\n" + e.Usage
	}
	return e.Msg
}

// File is a parsed cmd: one or more statements, in order.
type File struct {
	Statements []Statement
	// Empty reports that cmd held nothing to run, which is the "where am I"
	// call: it is legal, cheap, and returns what is available.
	Empty bool
}

// ops is the directive set. It is the whole user-facing grammar in one table,
// so `help`, the error usage lines and the parser cannot disagree.
var ops = []Op{
	{Name: "run", Kind: KindPayload, Target: true, MinArgs: 1,
		Usage: "run <handle> <payload>   |   payload: 原样交给 shell，协议不分词"},
	{Name: "type", Kind: KindPayload, Target: true, MinArgs: 1,
		Usage: "type <handle> <payload>   |   只打字，不回车"},
	{Name: "key", Kind: KindFlags, Target: true, MinArgs: 1, Flags: []string{"repeat"},
		Usage: "key <handle> <key> [--repeat N]"},
	{Name: "read", Kind: KindFlags, Target: true,
		Flags:     []string{"cursor", "from-line", "tail", "stream", "raw", "timeout", "max-bytes"},
		Valueless: []string{"raw"},
		Usage:     "read <handle> [--cursor N|--from-line N|--tail N] [--stream o|e] [--raw]",
		Detail:    readDetail},
	{Name: "pipe", Kind: KindPayload, Target: true,
		Flags: []string{"argv", "name", "on-exit", "rows", "cols"}, RestValue: []string{"argv"},
		Usage:  "pipe <entry|session> [--name ...] [--on-exit keep|close] [--argv <command> [args…]]",
		Detail: argvDetail},
	{Name: "pty", Kind: KindPayload, Target: true,
		Flags: []string{"argv", "name", "on-exit", "size"}, RestValue: []string{"argv"},
		Usage:  "pty <entry|session> [--name ...] [--size 24x80] [--argv <command> [args…]]",
		Detail: argvDetail},
	{Name: "ps", Kind: KindFlags, Flags: []string{"shells", "all"}, Valueless: []string{"shells", "all"},
		Usage: "ps [--shells] [--all]"},
	{Name: "info", Kind: KindFlags, Target: true,
		Usage: "info <session|handle>"},
	{Name: "resize", Kind: KindFlags, Target: true, MinArgs: 2,
		Usage: "resize <handle> <rows> <cols>"},
	{Name: "close", Kind: KindFlags, Target: true, Flags: []string{"force", "grace"}, Valueless: []string{"force"},
		Usage:  "close [--force] [--grace N] <handle|session>",
		Detail: closeDetail},
	{Name: "rm", Kind: KindFlags, Target: true,
		Usage: "rm <session>"},
	{Name: "span", Kind: KindFlags, Target: true,
		Usage:  "span <handle>",
		Detail: spanDetail},
	{Name: "reader", Kind: KindFlags, Target: true, Flags: []string{"drop"},
		Usage: "reader <handle>   |   reader <handle> --drop <id>"},
	{Name: "watch", Kind: KindFlags, Target: true, TargetOptional: true,
		Flags:     []string{"event", "for", "off", "list", "channel"},
		Valueless: []string{"list"},
		Usage:     "watch <handle> [--event output|exit|silence] [--for N] [--channel resource|sampling]   |   watch --off <rule>   |   watch --list",
		Detail:    watchDetail},
	{Name: "notify", Kind: KindPayload, Flags: []string{"level", "sticky", "on", "title", "for"}, Valueless: []string{"sticky"},
		Usage:  "notify [--level info|success|warn|error] [--sticky] [--on <session>] <body>",
		Detail: notifyDetail},
	{Name: "detect", Kind: KindFlags,
		Usage: "detect"},
	{Name: "fwd", Kind: KindFlags,
		Flags: []string{"action", "session", "remote-host", "remote-port", "local-port", "local-host", "forward-id"},
		Usage: "fwd --action local|remote|dynamic|list|close … <session>"},
	{Name: "file", Kind: KindFlags, Target: true,
		Flags: []string{"action", "remote-path", "from-path", "to-path", "local-path", "data", "mode",
			"offset", "length", "local-offset", "target", "link-path", "existing-path", "new-path",
			"uid", "gid", "atime", "mtime", "size", "response-headers"},
		Usage: "file <session> --action read|write|stat|delete|rename|mkdir|urls|perm|link|fs|getwd …"},
	{Name: "profile", Kind: KindFlags,
		Flags: []string{"action", "name", "host", "user", "port", "password", "private-key", "key-passphrase",
			"trust-unknown-host", "known-hosts", "dial-timeout-seconds", "proxy", "description",
			"default-shell", "default-mode", "default-approval", "jump-host", "jump-user", "jump-port", "source"},
		Valueless: []string{"trust-unknown-host", "default-approval"},
		Usage:     "profile list|create|edit|copy|delete …",
		Detail:    profileFileFormat},
	{Name: "ext", Kind: KindFlags,
		Flags: []string{"action", "name", "kind", "target", "description", "command", "args", "url", "headers",
			"mode", "rows", "cols", "allow", "paths", "notes", "enabled", "forwards"},
		Valueless: []string{"enabled"},
		Usage:     "ext list|get|create|update|delete|retry …"},
	{Name: "mcp", Kind: KindPayload,
		Usage: "mcp open <url> [--sse]   |   mcp tools|send|recv|call|close <handle> …"},
	{Name: "alias", Kind: KindPayload,
		Usage: "alias <name> <directive>   |   alias   |   alias <name> --off"},
	{Name: "help", Kind: KindFlags,
		Usage: "help [directive]"},
}

// watchDetail is the waiting rule, kept where a caller looks it up: the agent
// that held a terminal at a sudo prompt polled read in a loop because nothing
// told it waiting on a human is a supported move rather than a hang.
const watchDetail = `Register an async wake-up rule on a handle and stop polling; you are woken when
it fires. Events: output (any new bytes), exit (the process ended), silence (N
seconds with no output). Human at a prompt (sudo, password, a confirmation):
notify the human FIRST, then watch --event output on that handle — silence
fires on nobody speaking, which is the state you are already in while you wait.
One task's rule, not the handle's: watch --off <rule> when it is answered.
watch wakes the Agent; notify reaches the human.

--channel picks how the wake-up travels: resource (a resources/updated
notification, which any client can receive) or sampling (a createMessage the
client must have declared support for). Omit it and the server decides.
Defaults: --event output, --for 3 seconds (silence), --channel omitted = sampling
when the client declared it, resource otherwise.`

// spanDetail decodes the one-letter statuses, which are terse by design (they are
// written to log.jsonl for every status change) and unreadable without this.
const spanDetail = `Session output index: time, byte offset and line numbers per span, so a window
can be read by position instead of by streaming. status: o output (the shell's
own bytes), a AI input, i human input, q approval asked, A approved input. i/q
mean a person operated; a means the agent did. Read a span's bytes with
read <handle> --cursor <offset>, or its lines with --from-line <line>.`

// notifyDetail is the rule for reaching the human, stated where a caller looks it
// up. It used to be the notify_user tool's description; with one tool there is no
// per-operation description to hang it on, and the rule is exactly the kind of
// thing `help` exists for: needed at the moment of use, noise before it.
const notifyDetail = `Toasts + a system notification on every open Web UI page; --on highlights that
session's card. Call it BEFORE asking the human for anything — password, sudo,
MFA, passphrase, confirmation, approval, a choice, interactive input — and when
a long task ends or fails. Blocking asks: --level warn|error --sticky. Once you
have asked, WAIT: never poll, guess or fake an answer. A reply with no open page
was delivered to nobody, so say the ask in your own reply too. Defaults: --level
info; the toast lasts 10 seconds unless --sticky.`

// readDetail states the defaults because "read" without a selector is the
// common call: whether that means "new bytes" or "everything" decides what the
// caller does next, and guessing it from silence is how loops get written.
const readDetail = `Defaults: the new bytes since the last read on a live channel; a closed channel
returns its tail. --stream o keeps stdout only, e keeps stderr only; the default
keeps both. --cursor N is a byte offset (end+1 from Content-Range), --from-line N
a complete line, --tail N the last N lines.`

// closeDetail states the kill deadline, which otherwise looks instantaneous.
const closeDetail = `--force terminates a whole session; a channel closes with or without it. --grace N
is how many seconds a process gets to exit before it is killed. Default: 5.`

// argvDetail is the --argv contract: a word list, not a command line. It is
// stated on `help pipe` because both failure modes are silent or come from the
// wrong layer: a dropped argument looks like the program ignoring input, and a
// shell operator in the value would be handed to the program as text.
const argvDetail = `--argv is a word list (argv), not a shell command line. Words are split with the
shell's quoting rules, but nothing is expanded: quote an argument that contains
spaces, e.g. pipe internal --argv sh -c 'echo hi'. Shell operators are not
interpreted — for 'cd /srv && make' pass a script: --argv sh -c 'cd /srv && make'.
Put --argv LAST: every word after it belongs to the program, so --name, --on-exit
and --size must come before it. Write it as you would in a terminal: wrapping the
whole argv in one pair of quotes makes it ONE program name
(--argv 'go build ./...' runs a program with spaces in its name). --on-exit
defaults to keep: the session survives its last shell; --on-exit close ends a
one-shot session with it.`

// profileFileFormat is the import/export file shape (Web UI batch import; HTTP
// GET/POST /api/connections/batch). It is stated here, once, because the agent is
// often the one asked to *produce* that text — a user pastes a list of hosts and
// wants an importable file — and a format spec duplicated in prose would drift
// from the parser the moment either is edited.
const profileFileFormat = `profile file format (the batch import/export shape):

  [[connections]]
  name = "prod"                 # letters/digits/_/- , max 64
  kind = "remote"
  host = "10.0.0.5"
  user = "deploy"
  password = "…"                # or private_key = "…"
  # optional: port, key_passphrase, trust_unknown_host, known_hosts,
  #           dial_timeout_seconds, proxy, description,
  #           default_shell, default_mode, default_approval

A bastion is NESTED — [connections.jump], and [connections.jump.jump] for a
deeper hop. The flat --jump-host style flags of this directive are NOT file
fields: a file using them parses cleanly and silently drops the bastion.
Export emits this shape and skips temporary profiles, so it re-imports
unchanged. "internal" is reserved, an existing name becomes name-2, and
credentials are write-only.`

// byName indexes ops.
var byName = func() map[string]Op {
	m := make(map[string]Op, len(ops))
	for _, op := range ops {
		m[op.Name] = op
	}
	return m
}()

// Ops returns the table, in help order.
func Ops() []Op { return ops }

// Lookup returns a directive's grammar.
func Lookup(name string) (Op, bool) {
	op, ok := byName[name]
	return op, ok
}

// Parse reads cmd into statements.
//
// Statements are separated by `;` and newlines — the shell's own sequencing —
// while `&&`, `||` and `|` stay INSIDE a statement's payload. That split is
// deliberate: `run h make && echo ok` is one command whose result the caller
// wants (and one reply slot), whereas `run a:1 'x'; run b:1 'y'` asks two
// questions and gets two slots.
func Parse(src string) (*File, error) {
	if strings.TrimSpace(src) == "" {
		return &File{Empty: true}, nil
	}
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, &Error{Msg: "cannot parse cmd: " + err.Error()}
	}
	out := &File{}
	for _, stmt := range file.Stmts {
		if stmt == nil || stmt.Cmd == nil {
			continue
		}
		parsed, ok, err := parseStatement(src, stmt)
		if err != nil {
			// The first failure wins: a cmd is one instruction, and reporting the
			// rest of a broken instruction teaches nothing. A directive the caller
			// defined itself (an alias) surfaces as an unknown-directive error
			// carrying Op+Rest, which is what lets the caller resolve it.
			return nil, err
		}
		if ok {
			out.Statements = append(out.Statements, parsed)
		}
	}
	if len(out.Statements) == 0 {
		return &File{Empty: true}, nil
	}
	return out, nil
}

// leadCall finds the first CallExpr of a statement, in source order.
//
// A statement can be a pipeline or a chain (`a | b`, `a && b`); the directive is
// whatever the FIRST command is, and everything from its payload onwards belongs
// to that directive. Walking down the left spine finds it without depending on
// the statement's shape.
func leadCall(cmd syntax.Command) *syntax.CallExpr {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		return c
	case *syntax.BinaryCmd:
		if left := leadCall(c.X.Cmd); left != nil {
			return left
		}
		return leadCall(c.Y.Cmd)
	case *syntax.IfClause:
		if len(c.Then) > 0 {
			return leadCall(c.Then[0].Cmd)
		}
	case *syntax.Block:
		if len(c.Stmts) > 0 {
			return leadCall(c.Stmts[0].Cmd)
		}
	}
	return nil
}

// parseStatement reads one statement. ok is false for a statement that is not a
// directive at all (an assignment, a redirection-only command) — those are left
// for the next statement rather than reported, because `cmd` is a shell string
// and a caller may legitimately begin with something else.
func parseStatement(src string, stmt *syntax.Stmt) (Statement, bool, error) {
	call := leadCall(stmt.Cmd)
	if call == nil || len(call.Args) == 0 {
		return Statement{}, false, nil
	}
	opWord := call.Args[0]
	name, ok := literalWord(src, opWord)
	if !ok {
		// e.g. "$X" as the directive: not a directive this parser can name.
		return Statement{}, false, nil
	}
	op, known := Lookup(name)
	if !known {
		// A caller that typed a name this build does not have is exactly who a
		// usage line helps, so the error carries the whole table rather than
		// making them ask for it in a second call.
		return Statement{}, false, &Error{
			Op:    name,
			Msg:   "unknown directive: " + name,
			Usage: "help",
			Rest:  restOfStatement(src, stmt),
		}
	}

	out := Statement{
		Op:    name,
		Flags: map[string]string{},
		Path:  strings.TrimSpace(src[stmt.Pos().Offset():stmt.End().Offset()]),
	}

	var rest []word
	for _, arg := range call.Args[1:] {
		text, _ := literalWord(src, arg)
		rest = append(rest, word{
			text:  text,
			lit:   wordLiteral(arg),
			start: arg.Pos().Offset(),
			end:   arg.End().Offset(),
		})
	}

	// Word 2 is the target, for the directives that take one. A flag here is
	// refused rather than read as a target: `key --repeat 3 a1f3:1 up` would
	// otherwise silently target "--repeat", and the error has to say why.
	if op.Target {
		switch {
		case len(rest) == 0:
			if !op.TargetOptional {
				return Statement{}, false, &Error{Op: name, Msg: "directive " + name + " needs a target", Usage: op.Usage}
			}
		case strings.HasPrefix(rest[0].text, "--"):
			if !op.TargetOptional {
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   "flags come after the target, not before it: " + name + " " + rest[0].text,
					Usage: op.Usage,
				}
			}
		default:
			out.Target = rest[0].text
			rest = rest[1:]
		}
	}

	switch op.Kind {
	case KindPayload, KindInterpreted:
		// Leading --name flags belong to the directive (`pipe mac --name x --argv
		// go test`); everything from the first non-flag word to the end of the
		// statement is the payload, raw — except a RestValue flag, whose value is
		// the rest of the statement itself.
		i := 0
		for i < len(rest) && strings.HasPrefix(rest[i].text, "--") {
			key, val := splitFlag(rest[i].text)
			if !allowedFlag(op, key) {
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   "unknown flag for " + name + ": --" + key,
					Usage: op.Usage,
				}
			}
			if restValueFlag(op, key) {
				if i+1 >= len(rest) || strings.HasPrefix(rest[i+1].text, "--") {
					return Statement{}, false, &Error{
						Op:    name,
						Msg:   "--" + key + " needs a command, e.g. " + name + " <target> --" + key + " sleep 5",
						Usage: op.Usage,
					}
				}
				// A known flag after --argv is the caller's flag, not the
				// program's argument. Feeding it to the program would be the
				// silent drop this directive exists to avoid.
				for _, w := range rest[i+1:] {
					if k, _ := splitFlag(w.text); strings.HasPrefix(w.text, "--") && allowedFlag(op, k) {
						return Statement{}, false, &Error{
							Op:    name,
							Msg:   "flags come before --" + key + ": --" + k + " would otherwise be passed to the program",
							Usage: op.Usage,
						}
					}
				}
				tail := src[rest[i+1].start:stmt.End().Offset()]
				if val != "" {
					val += " " + tail
				} else {
					val = tail
				}
				out.Flags[key] = strings.TrimSpace(val)
				i = len(rest)
				break
			}
			if val == "" && !valuelessFlag(op, key) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1].text, "--") {
				val = rest[i+1].lit
				i++
			}
			out.Flags[key] = val
			i++
		}
		if i == len(rest) {
			if op.MinArgs > 0 {
				return Statement{}, false, &Error{Op: name, Msg: "directive " + name + " needs a payload", Usage: op.Usage}
			}
			break
		}
		if len(op.RestValue) > 0 {
			return Statement{}, false, &Error{
				Op:    name,
				Msg:   name + " takes its program via --" + op.RestValue[0] + ", not a bare payload: " + name + " <target> --" + op.RestValue[0] + " <command> [args…]",
				Usage: op.Usage,
			}
		}
		start, end := rest[i].start, stmt.End().Offset()
		if op.Kind == KindInterpreted {
			// An interpreted payload is a NAME, not a command line: it stops at
			// that word, because more words follow it (`alias jira mcp open …`).
			end = rest[i].end
			out.Args = append(out.Args, texts(rest[i+1:])...)
		}
		out.Payload = src[start:end]

	case KindFlags:
		for i := 0; i < len(rest); i++ {
			w := rest[i]
			if !strings.HasPrefix(w.text, "--") {
				out.Args = append(out.Args, w.text)
				continue
			}
			key, val := splitFlag(w.text)
			if !allowedFlag(op, key) {
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   "unknown flag for " + name + ": --" + key,
					Usage: op.Usage,
				}
			}
			if val == "" && !valuelessFlag(op, key) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1].text, "--") {
				val = rest[i+1].lit
				i++
			}
			out.Flags[key] = val
		}
	}

	if op.Kind == KindFlags && len(out.Args) < op.MinArgs {
		return Statement{}, false, &Error{
			Op:    name,
			Msg:   "directive " + name + " needs " + fmt.Sprint(op.MinArgs) + " argument(s), got " + fmt.Sprint(len(out.Args)),
			Usage: op.Usage,
		}
	}
	return out, true, nil
}

// word is a parsed argument plus its byte range in the source, so a payload can
// be SLICED instead of re-joined from parts.
//
// lit is the shell's own reading of the word — unquoted, but NOT expanded (no
// `$HOME`, no `$(cmd)`): the parser is forbidden from evaluating. A flag VALUE is
// taken from lit, because `--argv go build ./...` means the program name and its
// arguments, not a string with quotes in it; a payload is taken from the slice,
// because the shell downstream must see the quotes.
type word struct {
	text  string
	lit   string
	start uint
	end   uint
}

// texts returns the words' text, for the positional arguments that follow an
// interpreted payload.
func texts(ws []word) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.lit)
	}
	return out
}

// restOfStatement is everything after the directive word, verbatim.
//
// The slice comes from the source, so it is byte-identical to what the caller
// wrote: this is the same rule the payload follows, and it exists for the one
// statement the parser could not parse — a caller-defined directive, whose meaning
// only the alias table knows.
func restOfStatement(src string, stmt *syntax.Stmt) string {
	start, end := int(stmt.Pos().Offset()), int(stmt.End().Offset())
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	text := src[start:end]
	if i := strings.IndexAny(text, " 	"); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// wordLiteral is the shell's reading of one word: the parts joined WITHOUT quotes,
// and without expansion. A word assembled from several parts has no single Lit, so
// each part contributes its own literal text.
func wordLiteral(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	if lit := w.Lit(); lit != "" || len(w.Parts) == 0 {
		return lit
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				}
			}
		default:
			// A construct the parser must not evaluate ($VAR, $(cmd), a process
			// substitution). It is kept VERBATIM so the shell downstream expands
			// it; the parser's job is to hand it over intact, never to guess.
			b.WriteString(partText(part))
		}
	}
	return b.String()
}

// partText renders an unexpanded part from its source text. The syntax package
// offers no literal for a parameter expansion (that would be evaluating it), so
// the source slice is the honest answer — quoted so the shell reads it as the
// single word it was.
func partText(part syntax.Node) string {
	switch p := part.(type) {
	case *syntax.ParamExp:
		return "${" + p.Param.Value + "}"
	case *syntax.CmdSubst:
		return "$(…)"
	default:
		return "…"
	}
}

// splitFlag reads --name and --name=value.
func splitFlag(word string) (key, val string) {
	trimmed := strings.TrimPrefix(word, "--")
	if key, val, ok := strings.Cut(trimmed, "="); ok {
		return key, val
	}
	return trimmed, ""
}

// restValueFlag reports whether a flag's value is the rest of the statement.
func restValueFlag(op Op, key string) bool {
	for _, f := range op.RestValue {
		if f == key {
			return true
		}
	}
	return false
}

// valuelessFlag reports whether a flag takes no value.
func valuelessFlag(op Op, key string) bool {
	for _, f := range op.Valueless {
		if f == key {
			return true
		}
	}
	return false
}

// allowedFlag reports whether the directive accepts a flag.
func allowedFlag(op Op, key string) bool {
	for _, f := range op.Flags {
		if f == key {
			return true
		}
	}
	return false
}

// literalWord recovers a word's text from the source slice.
//
// Word.Lit() is empty whenever the word is assembled from several parts
// (`'a'"b"'c'` is three), so slicing the source is the only reading that survives
// quoting. For a word that IS a plain literal, the slice is that literal.
func literalWord(src string, w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	start, end := w.Pos().Offset(), w.End().Offset()
	if end > uint(len(src)) || start > end {
		return "", false
	}
	raw := src[start:end]
	// Strip nothing: the target and the flag names must be plain, and a caller
	// that quoted a target gets the quotes back (which then fails to resolve,
	// visibly, instead of silently matching). The payload never comes through
	// here — it is sliced.
	if strings.ContainsAny(raw, "'\"") {
		return "", false
	}
	return raw, true
}

// SplitArgv turns a --argv value into the executable and its arguments.
//
// Words are read with the shell's own grammar, so quoting groups the way a
// caller expects (`sh -c "echo hi"` is three words, the third keeping its
// space), but nothing is evaluated. There is no second rule for a value that is
// one quoted word: `--argv 'go build ./...'` is ONE word, exactly as it would be
// in a terminal.
//
// A value that is a command line rather than a word list — a second statement,
// a pipeline, a redirection, an assignment — is an error, not something to feed
// to the program as text.
func SplitArgv(raw string) (string, []string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, nil
	}
	words, err := shellWords(raw)
	if err != nil {
		return "", nil, err
	}
	if len(words) == 0 {
		return "", nil, nil
	}
	return words[0], words[1:], nil
}

// shellWords parses raw as a single simple command and returns each word's
// literal, with quotes removed and expansions left unexpanded.
func shellWords(raw string) ([]string, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(raw), "")
	if err != nil {
		return nil, fmt.Errorf("cannot read --argv: %v", err)
	}
	notArgv := fmt.Errorf("%q is a command line, not an argv: one program per --argv (use sh -c or a script for shell syntax)", raw)
	if len(file.Stmts) != 1 || file.Stmts[0].Background {
		return nil, notArgv
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 || len(call.Assigns) > 0 || len(file.Stmts[0].Redirs) > 0 {
		return nil, notArgv
	}
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		words = append(words, wordLiteral(w))
	}
	return words, nil
}

// Names lists the directive names, sorted, for a help listing that cannot drift
// from the table.
func Names() []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Name)
	}
	sort.Strings(out)
	return out
}
