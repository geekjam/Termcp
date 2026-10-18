package session

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Restriction trims what may act on a session, across every surface that can
// act on one: the MCP tools, the REST endpoints, and the WebSocket the browser
// terminal speaks. It is the mechanism behind extensions: an extension is an
// ordinary session whose capabilities were narrowed at creation time, so a
// handle handed to an agent can only do what the extension's author decided —
// and nothing the author did not list is silently reachable.
//
// Enforcing it on one transport only would make the whitelist a matter of which
// door the caller picked, so each surface asks these same questions rather than
// restating the rule: internal/mcp's toolGuard and internal/webui's
// restRefuseTool/restRefusePaths are thin wrappers over AllowsTool/AllowsPath.
//
// Both halves are whitelists and the zero value means unrestricted: an ordinary
// session carries a Restriction{} and every tool behaves exactly as before.
// Building one through NewRestriction is the only supported route, because path
// patterns must be validated once, at extension-load time, rather than failing
// mid-conversation on a pattern nobody has looked at.
type Restriction struct {
	allow []string
	paths []pathRule
}

// NewRestriction copies the tool whitelist and compiles every path pattern so a
// malformed one is reported when an extension is saved, not when an agent first
// reaches for a file. Blank patterns are dropped rather than counted as rules.
func NewRestriction(allow []string, paths []string) (Restriction, error) {
	r := Restriction{allow: append([]string(nil), allow...)}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		rule, err := compilePathRule(p)
		if err != nil {
			return Restriction{}, err
		}
		r.paths = append(r.paths, rule)
	}
	return r, nil
}

// AllowsTool reports whether a tool may run against this session. An empty allow
// list is the unrestricted default; any other list is an exact-name whitelist,
// so a restricted session is a strict subset of an unrestricted one.
func (r Restriction) AllowsTool(name string) bool {
	if len(r.allow) == 0 {
		return true
	}
	for _, a := range r.allow {
		if a == name {
			return true
		}
	}
	return false
}

// AllowsPath reports whether a remote path is inside the whitelist. An empty rule
// set is the unrestricted default; otherwise the first matching rule grants it
// and no match denies it. Rules are alternatives (OR), not a conjunction: pair a
// tree entry with a glob by listing both.
func (r Restriction) AllowsPath(p string) bool {
	if len(r.paths) == 0 {
		return true
	}
	clean := path.Clean(p)
	for _, rule := range r.paths {
		switch rule.kind {
		case ruleTree:
			// A component-boundary prefix, not a string one: granting /srv/data must
			// never let an agent reach /srv/database by name adjacency.
			if clean == rule.literal || strings.HasPrefix(clean, rule.literal+"/") {
				return true
			}
		case ruleGlob:
			if ok, _ := path.Match(rule.glob, clean); ok {
				return true
			}
		case ruleRegexp:
			if rule.re.MatchString(clean) {
				return true
			}
		}
	}
	return false
}

// ForbiddenTool renders the refusal for a tool a session's whitelist does not
// carry. Both surfaces that can act on a session — the MCP tool guard and the
// REST handlers — answer with this text, so a caller who meets the whitelist on
// either one reads the same sentence and is pointed at the same remedy.
func ForbiddenTool(toolName string) string {
	return "tool " + toolName + " is not permitted on this session: it is an extension session whose capability list does not include it. Use extension(action=get) to see what this session was created to allow."
}

// ForbiddenPath renders the refusal for a path argument outside the whitelist.
// arg is the argument that carried the path, so the caller learns which of a
// file tool's several paths was the offending one.
func ForbiddenPath(arg, path string) string {
	return "path argument " + arg + " is outside the directories this extension session may reach: " + path + ". Use extension(action=get) to see the allowed paths."
}

// ruleKind selects the matcher a pathPattern compiled into.
type ruleKind int

const (
	ruleTree ruleKind = iota
	ruleGlob
	ruleRegexp
)

// pathRule is one entry of the path whitelist. Exactly one field is meaningful,
// per kind.
type pathRule struct {
	kind    ruleKind
	literal string         // ruleTree: the cleaned directory whose subtree is granted
	glob    string         // ruleGlob: a slash-based pattern for path.Match
	re      *regexp.Regexp // ruleRegexp: the compiled expression
}

// compilePathRule picks a matcher from the pattern's own shape:
//
//	~/skills/foo/   no magic characters → a directory subtree
//	*.md            * ? [ present       → a slash-based glob
//	re:^/srv/.*\.md$ re: prefix         → a regular expression
//
// Globs and trees are slash-based — the form SFTP reports a remote POSIX path
// in — regardless of the termcp host's own OS, and path.Match has no **, so a
// recursive match is expressed as a tree entry rather than a glob. The regex
// branch is safe to hand to an agent that writes its own patterns: Go's regexp
// is RE2, linear-time with no catastrophic backtracking, so a pattern cannot
// burn CPU here the way a backtracking engine would let it.
func compilePathRule(p string) (pathRule, error) {
	if rest, ok := strings.CutPrefix(p, "re:"); ok {
		re, err := regexp.Compile(rest)
		if err != nil {
			return pathRule{}, fmt.Errorf("invalid regex %q: %w", p, err)
		}
		return pathRule{kind: ruleRegexp, re: re}, nil
	}
	if strings.ContainsAny(p, "*?[") {
		if _, err := path.Match(p, "probe"); err != nil {
			return pathRule{}, fmt.Errorf("invalid glob %q: %w", p, err)
		}
		return pathRule{kind: ruleGlob, glob: p}, nil
	}
	return pathRule{kind: ruleTree, literal: path.Clean(p)}, nil
}
