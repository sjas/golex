package main

import (
	"fmt"
	"io"
	"regexp"
	goparser "go/parser"
	gotoken "go/token"
	goast "go/ast"
	goprinter "go/printer"
	"bytes"
	"strings"
)

type LexFile struct {
	packageName string

	prologue string

	actionInline    string
	startConditions map[string]LexStartCondition
	rules           []LexRule

	epilogue string
}

type LexStartCondition struct {
	num  int
	excl bool
}

type LexRule struct {
	startConditions []string
	pattern         string
	trailingPattern string
	code            string
}

func NewLexFile() *LexFile {
	return &LexFile{startConditions: make(map[string]LexStartCondition),
		rules: make([]LexRule, 0, 15)}
}

type outf struct {
	io.Writer
}

func (w outf) W(s string) {
	w.Write([]byte(s))
}
func (w outf) Wf(s string, args ...interface{}) {
	w.W(fmt.Sprintf(s, args...))
}

func (lf *LexFile) WriteGo(out io.Writer) {
	w := outf{out}

	w.Wf("// Generated by golex\npackage %s\n\n", lf.packageName)
	w.W(IMPORTS)
	w.W(lf.prologue)
	w.W(PROLOGUE)

	for k, v := range lf.startConditions {
		w.Wf("var %s yystartcondition = %d\n", k, v.num)
	}

	w.W(`var yystartconditionexclmap = map[yystartcondition]bool{`)
	for k, v := range lf.startConditions {
		w.Wf("%s: %v, ", k, v.excl)
	}
	w.W("}\n")

	w.W(`var yyrules []yyrule = []yyrule{`)
	for _, v := range lf.rules {
		// fmt.Fprintf(os.Stdout, "compiling __%s__\n", v.pattern)
		// m := regexp.MustCompile(v.pattern).FindStringSubmatch("")
		// if m != nil {
			// fmt.Fprintf(os.Stdout, "WARNING: pattern /%s/ matches empty string \"\"\n", v.pattern)
		// }

		w.Wf("{regexp.MustCompile(%s), ", quoteRegexp(v.pattern))
		if tp := v.trailingPattern; tp != "" {
			w.Wf("regexp.MustCompile(%s), ", quoteRegexp(tp))
		} else {
			w.W("nil, ")
		}

		w.W("[]yystartcondition{")
		for _, sc := range v.startConditions {
			if sc == "*" {
				w.W("-1, ")
			} else {
				w.Wf("%s, ", sc)
			}
		}
		w.W("}, ")

		if v.pattern[0] == '^' {
			w.W("true, ")
		} else {
			w.W("false, ")
		}

		w.W(codeToAction(v.code))
		w.W("}, ")
	}
	w.W("}\n")

	w.W("func yyactioninline(BEGIN func(yystartcondition)) {" + lf.actionInline + "}\n")

	w.W(lf.epilogue)
}

type codeToActionVisitor struct{}

func (ctav *codeToActionVisitor) Visit(node goast.Node) goast.Visitor {
	exprs, ok := node.(*goast.ExprStmt)
	if ok {
		// Transform ECHO, REJECT to yyECHO(), yyREJECT().
		rid, rok := exprs.X.(*goast.Ident)
		if rok && (rid.Name == "ECHO" || rid.Name == "REJECT") {
			rid.Name = "yy" + rid.Name
			exprs.X = &goast.CallExpr{Fun: exprs.X,
				Args: nil}
		}

		// Transform BEGIN(...) into yyBEGIN(...).
		rcall, rok := exprs.X.(*goast.CallExpr)
		if rok {
			rident, rok := rcall.Fun.(*goast.Ident)
			if rok && rident.Name == "BEGIN" {
				rident.Name = "yyBEGIN"
			}
		}

		return ctav
	}

	// Transform 'return 1' into 'return yyactionreturn{1, yyRT_USER_RETURN}'. Take special
	// effort not to touch existing 'return yyactionreturn{...}' statements.
	retstmt, ok := node.(*goast.ReturnStmt)
	if ok {
		if len(retstmt.Results) == 1 {
			r := retstmt.Results[0]
			_, ok := r.(*goast.CompositeLit)

			if !ok {
				// Wrap it.
				retstmt.Results[0] = &goast.CompositeLit{Type: &goast.Ident{Name: "yyactionreturn"},
					Elts: []goast.Expr{r, &goast.Ident{Name: "yyRT_USER_RETURN"}}}
			}
		}
	}

	return ctav
}

func codeToAction(code string) string {
	fs := gotoken.NewFileSet()

	newCode := `
func() (yyar yyactionreturn) {
	defer func() {
		if r := recover(); r != nil {
			if r != "yyREJECT" {
				panic(r)
			}
			yyar.returnType = yyRT_REJECT
		}
	}()
		
	` + code + `;
	return yyactionreturn{0, yyRT_FALLTHROUGH}
}`

	expr, _ := goparser.ParseExpr(fs, "", newCode)

	fexp := expr.(*goast.FuncLit)

	ctav := &codeToActionVisitor{}
	goast.Walk(ctav, fexp)

	result := bytes.NewBuffer(make([]byte, 0, len(code)*2))
	goprinter.Fprint(result, fs, fexp)

	return result.String()
}

func isOctalDigit(d uint8) bool {
	return d >= '0' && d <= '7'
}

func isHexDigit(d uint8) bool {
	return (d >= '0' && d <= '9') || (d >= 'a' && d <= 'f') || (d >= 'A' && d <= 'F')
}

func formatMetaChar(d int) string {
	if d < 32 {
		return fmt.Sprintf("\\x%02x", d)
	} 
	return strings.Replace(strings.Replace(regexp.QuoteMeta(string(d)), "\\", "\\\\", -1), "\"", "\\\"", -1)
}

// quoteRegexp prepares a regular expression for insertion into a Go source
// as a string suitable for use as argument to regexp.(Must)?Compile.
func quoteRegexp(re string) (out string) {
	out = "\""

	skip := 0
	for i, c := range re {
		if skip > 0 {
			skip--
			continue
		}

		switch c {
		case '"': out += "\\\""
		case '\\':
			if len(re) == i+1 {
				// This is the last character.
				out += "\\\\"
			} else if isOctalDigit(re[i+1]) {
				// The next character is an octal digit.
				if len(re) >= i+4 && isOctalDigit(re[i+2]) && isOctalDigit(re[i+3]) {
					// .. and there two more octal digits beyond that.
					var oct int
					fmt.Sscanf(re[i+1:i+4], "%o", &oct)
					out += formatMetaChar(oct)
					skip += 3
				} else {
					// There's no valid 3-digit octal here ..
					if re[i+1] == '0' {
						// .. so treat the leading \0 as a NUL.
						out += "\\000"
						skip++
					} else {
						// .. so escape the leading \.
						out += "\\\\"
					}
				}
			} else if re[i+1] == 'x' || re[i+1] == 'X' {
				// The next character ~ [xX].
				if len(re) >= i+4 && isHexDigit(re[i+2]) && isHexDigit(re[i+3]) {
					// .. and there are two hex digits beyond that.
					var hex int
					fmt.Sscanf(re[i+2:i+4], "%x", &hex)
					out += formatMetaChar(hex)
					skip += 3
				} else {
					// There's no valid hex sequence here.
					out += "\\\\"
				}
			} else if re[i+1] == '\\' {
				out += "\\\\\\\\"
				skip++
			} else {
				out += "\\\\"
			}
		default: out += string(c)
		}
	}

	out += "\""

	return
}

const IMPORTS = `
import (
	"regexp"
	"io"
	"bufio"
	"os"
	"sort"
)
`

const PROLOGUE = `
var yyin io.Reader = os.Stdin
var yyout io.Writer = os.Stdout

type yyrule struct {
	regexp     *regexp.Regexp
	trailing   *regexp.Regexp
	startConds []yystartcondition
	sol        bool
	action     func() yyactionreturn
}

type yyactionreturn struct {
	userReturn int
	returnType yyactionreturntype
}

type yyactionreturntype int
const (
	yyRT_FALLTHROUGH yyactionreturntype = iota
	yyRT_USER_RETURN
	yyRT_REJECT
)

var yydata string = ""
var yyorig string
var yyorigidx int

var yytext string = ""
var yytextrepl bool = true
func yymore() {
	yytextrepl = false
}

func yyBEGIN(state yystartcondition) {
	YY_START = state
}

func yyECHO() {
	yyout.Write([]byte(yytext))
}

func yyREJECT() {
	panic("yyREJECT")
}

var yylessed int
func yyless(n int) {
	yylessed = len(yytext) - n
}

func unput(c uint8) {
	yyorig = yyorig[:yyorigidx] + string(c) + yyorig[yyorigidx:]
	yydata = yydata[:len(yytext)-yylessed] + string(c) + yydata[len(yytext)-yylessed:]
}

func input() int {
	if len(yyorig) <= yyorigidx {
		return EOF
	}
	c := yyorig[yyorigidx]
	yyorig = yyorig[:yyorigidx] + yyorig[yyorigidx+1:]
	yydata = yydata[:len(yytext)-yylessed] + yydata[len(yytext)-yylessed+1:]
	return int(c)
}

var EOF int = -1
type yystartcondition int

var INITIAL yystartcondition = 0
var YY_START yystartcondition = INITIAL

type yylexMatch struct {
	index	  int
	matchFunc func() yyactionreturn
	sortLen   int
	advLen    int
}

type yylexMatchList []yylexMatch

func (ml yylexMatchList) Len() int {
	return len(ml)
}

func (ml yylexMatchList) Less(i, j int) bool {
	return ml[i].sortLen > ml[j].sortLen && ml[i].index > ml[j].index
}

func (ml yylexMatchList) Swap(i, j int) {
	ml[i], ml[j] = ml[j], ml[i]
}

func yylex() int {
	reader := bufio.NewReader(yyin)

	for {
		line, err := reader.ReadString('\n')
		if len(line) == 0 && err == os.EOF {
			break
		}

		yydata += line
	}

	yyorig = yydata
	yyorigidx = 0

	yyactioninline(yyBEGIN)

	for len(yydata) > 0 {
		matches := yylexMatchList(make([]yylexMatch, 0, 6))
		excl := yystartconditionexclmap[YY_START]

		for i, v := range yyrules {
			sol := yyorigidx == 0 || yyorig[yyorigidx-1] == '\n'

			if v.sol && !sol {
				continue
			}

			// Check start conditions.
			ok := false

			// YY_START or '*' must feature in v.startConds
			for _, c := range v.startConds {
				if c == YY_START || c == -1 {
					ok = true
					break
				}
			}

			if !excl {
				// If v.startConds is empty, this is also acceptable.
				if len(v.startConds) == 0 {
					ok = true
				}
			}

			if !ok {
				continue
			}

			idxs := v.regexp.FindStringIndex(yydata)
			if idxs != nil && idxs[0] == 0 {
				// Check the trailing context, if any.
				checksOk := true
				sortLen := idxs[1]
				advLen := idxs[1]

				if v.trailing != nil {
					tridxs := v.trailing.FindStringIndex(yydata[idxs[1]:])
					if tridxs == nil || tridxs[0] != 0 {
						checksOk = false
					} else {
						sortLen += tridxs[1]
					}
				}

				if checksOk {
					matches = append(matches, yylexMatch{i, v.action, sortLen, advLen})
				}
			}
		}

		if yytextrepl {
			yytext = ""
		}

		sort.Sort(matches)

	tryMatch:
		if len(matches) == 0 {
			yytext += yydata[:1]
			yydata = yydata[1:]
			yyorigidx += 1

			yyout.Write([]byte(yytext))
		} else {
			m := matches[0]
			yytext += yydata[:m.advLen]
			yyorigidx += m.advLen

			yytextrepl, yylessed = true, 0
			ar := m.matchFunc()

			if ar.returnType != yyRT_REJECT {
				yydata = yydata[m.advLen-yylessed:]
				yyorigidx -= yylessed
			}

			switch ar.returnType {
			case yyRT_FALLTHROUGH:
				// Do nothing.
			case yyRT_USER_RETURN:
				return ar.userReturn
			case yyRT_REJECT:
				matches = matches[1:]
				yytext = yytext[:len(yytext)-m.advLen]
				yyorigidx -= m.advLen
				goto tryMatch
			}
		}
	}

	return 0
}
`
