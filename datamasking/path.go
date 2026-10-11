package datamasking

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ohler55/ojg/jp"
)

// Field expressions are written in the dialect of jsonpath-ng, the library
// Powertools Python parses them with, and evaluated by ojg: the parser below
// accepts the part of that dialect Powertools documents and tests, and
// rejects the rest, so that an expression never means something else here
// than it does in Python. The package documentation lists the dialect.

// maxPathDepth bounds the nesting of parentheses and filters, so that a
// hostile expression cannot exhaust the stack.
const maxPathDepth = 32

var (
	errPathSyntax      = errors.New("syntax error")
	errPathUnsupported = errors.New("unsupported syntax")
	errPathTooDeep     = errors.New("expression nested too deeply")
)

// compiledPath is a parsed expression; a nil expr selects the root.
type compiledPath struct {
	expr jp.Expr
}

func compilePath(path string) (compiledPath, error) {
	p := &pathParser{src: path}
	expr, err := p.parse()
	if err != nil {
		return compiledPath{}, err
	}
	return compiledPath{expr: expr}, nil
}

type pathParser struct {
	src   string
	pos   int
	depth int
}

func (p *pathParser) parse() (jp.Expr, error) {
	p.skipSpace()
	var x jp.Expr
	if p.peek() == '$' {
		p.pos++
	} else {
		// Without a root, the expression starts with a member or a bracket.
		frags, err := p.member()
		if err != nil {
			return nil, err
		}
		x = append(x, frags...)
	}
	x, err := p.segments(x)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.done() {
		return nil, p.errorf(errPathSyntax, "unexpected %q", p.rest())
	}
	return x, nil
}

// segments reads the .member, ..member and [bracket] steps that follow.
func (p *pathParser) segments(x jp.Expr) (jp.Expr, error) {
	for {
		p.skipSpace()
		var frags jp.Expr
		var err error
		switch {
		case strings.HasPrefix(p.src[p.pos:], ".."):
			p.pos += 2
			p.skipSpace()
			frags, err = p.member()
			frags = append(jp.Expr{jp.Descent('.')}, frags...)
		case p.peek() == '.':
			p.pos++
			p.skipSpace()
			frags, err = p.member()
		case p.peek() == '[':
			frags, err = p.bracket()
		default:
			return x, nil
		}
		if err != nil {
			return nil, err
		}
		x = append(x, frags...)
	}
}

// member reads a name, a comma-separated list of names, a quoted name, a
// number used as a name, '*', or a bracket.
func (p *pathParser) member() (jp.Expr, error) {
	switch p.peek() {
	case '[':
		return p.bracket()
	case '*':
		p.pos++
		return jp.Expr{jp.Wildcard('*')}, nil
	case '$':
		return nil, p.errorf(errPathUnsupported, "root inside an expression")
	}
	names, err := p.names()
	if err != nil {
		return nil, err
	}
	return namesFrag(names), nil
}

// names reads one name or a comma-separated list of them.
func (p *pathParser) names() ([]string, error) {
	var names []string
	for {
		p.skipSpace()
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		p.skipSpace()
		if p.peek() != ',' {
			return names, nil
		}
		p.pos++
	}
}

func namesFrag(names []string) jp.Expr {
	if len(names) == 1 {
		return jp.Expr{jp.Child(names[0])}
	}
	union := make(jp.Union, 0, len(names))
	for _, n := range names {
		union = append(union, n)
	}
	return jp.Expr{union}
}

// name reads an identifier, a quoted name or a number used as a name.
func (p *pathParser) name() (string, error) {
	switch c := p.peek(); {
	case c == '\'' || c == '"':
		return p.quoted()
	case c == '-' || isDigit(c):
		lit := p.integerLiteral()
		if lit == "" || lit == "-" {
			return "", p.errorf(errPathSyntax, "expected a name")
		}
		return lit, nil
	}
	if id := p.identifier(); id != "" {
		return id, nil
	}
	if p.done() {
		return "", p.errorf(errPathSyntax, "unexpected end of expression")
	}
	return "", p.errorf(errPathSyntax, "unexpected %q", p.rest())
}

// identifier reads [letter _][letter digit _ @ -]*, jsonpath-ng's ID.
func (p *pathParser) identifier() string {
	start := p.pos
	for !p.done() {
		r, size := utf8.DecodeRuneInString(p.src[p.pos:])
		ok := unicode.IsLetter(r) || r == '_'
		if p.pos > start {
			ok = ok || unicode.IsDigit(r) || r == '@' || r == '-'
		}
		if !ok {
			break
		}
		p.pos += size
	}
	return p.src[start:p.pos]
}

// quoted reads a single- or double-quoted name, where a backslash makes the
// next character literal.
func (p *pathParser) quoted() (string, error) {
	quote := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for !p.done() {
		c := p.src[p.pos]
		switch c {
		case quote:
			p.pos++
			return b.String(), nil
		case '\\':
			if p.pos+1 >= len(p.src) {
				return "", p.errorf(errPathSyntax, "unterminated quoted name")
			}
			r, size := utf8.DecodeRuneInString(p.src[p.pos+1:])
			b.WriteRune(r)
			p.pos += 1 + size
		default:
			r, size := utf8.DecodeRuneInString(p.src[p.pos:])
			b.WriteRune(r)
			p.pos += size
		}
	}
	return "", p.errorf(errPathSyntax, "unterminated quoted name")
}

// integerLiteral reads -?[0-9]*.
func (p *pathParser) integerLiteral() string {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	for isDigit(p.peek()) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// bracket reads [*], [n], [n,m], [a:b:c], [name,...] or [?filter].
func (p *pathParser) bracket() (jp.Expr, error) {
	p.pos++ // [
	p.skipSpace()
	var frags jp.Expr
	var err error
	switch c := p.peek(); {
	case c == '*':
		p.pos++
		frags = jp.Expr{jp.Wildcard('*')}
	case c == '?':
		p.pos++
		frags, err = p.filter()
	case c == '-' || isDigit(c) || c == ':':
		frags, err = p.indexes()
	default:
		var names []string
		if names, err = p.names(); err == nil {
			frags = namesFrag(names)
		}
	}
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.peek() != ']' {
		return nil, p.errorf(errPathSyntax, "expected ']'")
	}
	p.pos++
	return frags, nil
}

// indexes reads n, n,m,... or a slice start:end:step with optional parts.
func (p *pathParser) indexes() (jp.Expr, error) {
	first, err := p.optionalInt()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.peek() == ':' {
		return p.slice(first)
	}
	if first == jp.SliceNotSet {
		return nil, p.errorf(errPathSyntax, "expected an index")
	}
	if p.peek() != ',' {
		return jp.Expr{jp.Nth(first)}, nil
	}
	union := jp.Union{int64(first)}
	for p.peek() == ',' {
		p.pos++
		p.skipSpace()
		n, err := p.optionalInt()
		if err != nil {
			return nil, err
		}
		if n == jp.SliceNotSet {
			return nil, p.errorf(errPathSyntax, "expected an index")
		}
		union = append(union, int64(n))
		p.skipSpace()
	}
	return jp.Expr{union}, nil
}

func (p *pathParser) slice(start int) (jp.Expr, error) {
	s := jp.Slice{start, jp.SliceNotSet, jp.SliceNotSet}
	for i := 1; i < 3 && p.peek() == ':'; i++ {
		p.pos++
		p.skipSpace()
		n, err := p.optionalInt()
		if err != nil {
			return nil, err
		}
		s[i] = n
		p.skipSpace()
	}
	if s[2] == 0 {
		return nil, p.errorf(errPathSyntax, "slice step cannot be zero")
	}
	return jp.Expr{s}, nil
}

// optionalInt reads an integer, or returns jp.SliceNotSet when there is
// none: ojg's own marker for a slice bound left out.
func (p *pathParser) optionalInt() (int, error) {
	lit := p.integerLiteral()
	if lit == "" {
		return jp.SliceNotSet, nil
	}
	n, err := strconv.ParseInt(lit, 10, 32)
	if err != nil || n == jp.SliceNotSet {
		return 0, p.errorf(errPathSyntax, "index %s out of range", lit)
	}
	return int(n), nil
}

// filter reads what follows [?: conditions joined by &, optionally in
// parentheses.
func (p *pathParser) filter() (jp.Expr, error) {
	eq, err := p.conditions()
	if err != nil {
		return nil, err
	}
	return jp.Expr{eq.Filter()}, nil
}

func (p *pathParser) conditions() (*jp.Equation, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxPathDepth {
		return nil, p.errorf(errPathTooDeep, "more than %d levels", maxPathDepth)
	}
	eq, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		switch {
		case p.peek() == '&' && p.peekAt(1) != '&':
			p.pos++
		case strings.HasPrefix(p.src[p.pos:], "&&") || strings.HasPrefix(p.src[p.pos:], "||") || p.peek() == '|':
			return nil, p.errorf(errPathUnsupported, "only & joins filter conditions")
		default:
			return eq, nil
		}
		right, err := p.term()
		if err != nil {
			return nil, err
		}
		eq = jp.And(eq, right)
	}
}

func (p *pathParser) term() (*jp.Equation, error) {
	p.skipSpace()
	if p.peek() == '(' {
		p.pos++
		eq, err := p.conditions()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.peek() != ')' {
			return nil, p.errorf(errPathSyntax, "expected ')'")
		}
		p.pos++
		return eq, nil
	}
	return p.condition()
}

// condition reads !path, path, or path OP literal, where path is relative
// to the element: @, @.a.b, or a.b.
func (p *pathParser) condition() (*jp.Equation, error) {
	negate := p.peek() == '!' && p.peekAt(1) != '='
	if negate {
		p.pos++
		p.skipSpace()
	}
	target, err := p.relativePath()
	if err != nil {
		return nil, err
	}
	get := jp.Get(target)
	if negate {
		return jp.Exists(get, jp.ConstBool(false)), nil
	}
	p.skipSpace()
	op := p.operator()
	if op == "" {
		return jp.Exists(get, jp.ConstBool(true)), nil
	}
	p.skipSpace()
	return p.comparison(get, op)
}

func (p *pathParser) relativePath() (jp.Expr, error) {
	x := jp.A()
	if p.peek() == '@' {
		p.pos++
	} else {
		names, err := p.names()
		if err != nil {
			return nil, err
		}
		x = append(x, namesFrag(names)...)
	}
	return p.segments(x)
}

var filterOperators = []string{"=~", "==", "<=", ">=", "!=", "=", "<", ">"}

func (p *pathParser) operator() string {
	for _, op := range filterOperators {
		if strings.HasPrefix(p.src[p.pos:], op) {
			p.pos += len(op)
			return op
		}
	}
	return ""
}

func (p *pathParser) comparison(get *jp.Equation, op string) (*jp.Equation, error) {
	value, err := p.literal()
	if err != nil {
		return nil, err
	}
	if op == "=~" {
		pattern, ok := value.(string)
		if !ok {
			return nil, p.errorf(errPathSyntax, "=~ needs a quoted pattern")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, p.errorf(errPathSyntax, "pattern %q: %v", pattern, err)
		}
		return jp.Regex(get, jp.ConstRegex(re)), nil
	}
	right := constant(value)
	switch op {
	case "==", "=":
		return jp.Eq(get, right), nil
	case "!=":
		return jp.Neq(get, right), nil
	case "<":
		return jp.Lt(get, right), nil
	case "<=":
		return jp.Lte(get, right), nil
	case ">":
		return jp.Gt(get, right), nil
	default: // >=
		return jp.Gte(get, right), nil
	}
}

func constant(v any) *jp.Equation {
	switch t := v.(type) {
	case int64:
		return jp.ConstInt(t)
	case float64:
		return jp.ConstFloat(t)
	case bool:
		return jp.ConstBool(t)
	case string:
		return jp.ConstString(t)
	default:
		// literal returns only the four types above.
		panic(fmt.Sprintf("datamasking: unexpected literal %T", v))
	}
}

// literal reads an integer, a decimal number, true, false, a quoted string
// or a bare identifier, which jsonpath-ng reads as a string.
func (p *pathParser) literal() (any, error) {
	switch c := p.peek(); {
	case c == '\'' || c == '"':
		return p.quoted()
	case c == '-' || isDigit(c):
		start := p.pos
		lit := p.integerLiteral()
		if p.peek() == '.' && isDigit(p.peekAt(1)) && lit != "-" {
			p.pos++
			for isDigit(p.peek()) {
				p.pos++
			}
			f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
			if err != nil {
				return nil, p.errorf(errPathSyntax, "number %q", p.src[start:p.pos])
			}
			return f, nil
		}
		n, err := strconv.ParseInt(lit, 10, 64)
		if err != nil {
			return nil, p.errorf(errPathSyntax, "number %q", lit)
		}
		return n, nil
	}
	id := p.identifier()
	switch id {
	case "":
		return nil, p.errorf(errPathSyntax, "expected a value")
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return id, nil
}

func (p *pathParser) done() bool { return p.pos >= len(p.src) }

func (p *pathParser) peek() byte { return p.peekAt(0) }

func (p *pathParser) peekAt(n int) byte {
	if p.pos+n < len(p.src) {
		return p.src[p.pos+n]
	}
	return 0
}

func (p *pathParser) rest() string {
	const show = 16
	if len(p.src)-p.pos > show {
		return p.src[p.pos:p.pos+show] + "..."
	}
	return p.src[p.pos:]
}

func (p *pathParser) skipSpace() {
	for p.peek() == ' ' || p.peek() == '\t' {
		p.pos++
	}
}

func (p *pathParser) errorf(kind error, format string, args ...any) error {
	return fmt.Errorf("%w at offset %d: %s", kind, p.pos, fmt.Sprintf(format, args...))
}
