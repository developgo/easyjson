package gen

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

const pkgWriter = "github.com/mailru/easyjson/jwriter"
const pkgLexer = "github.com/mailru/easyjson/jlexer"

// FieldNamer defines a policy for generating names for struct fields.
type FieldNamer interface {
	GetJSONFieldName(t reflect.Type, f reflect.StructField) string
}

// Generator generates the requested marshallers/unmarshallers.
type Generator struct {
	out *bytes.Buffer

	pkgName   string
	pkgPath   string
	buildTags string

	varCounter int

	omitEmpty bool
	namer     FieldNamer

	// package path to local alias map for tracking imports
	imports map[string]string

	// types that marshallers were requested for by user
	marshallers map[reflect.Type]bool

	// types that encoders were already generated for
	typesSeen map[reflect.Type]bool

	// types that encoders were requested for (e.g. by encoders of other types)
	typesUnseen []reflect.Type

	// function name to relevant type maps to track names of de-/encoders in
	// case of a name clash or unnamed structs
	functionNames map[string]reflect.Type
}

// NewGenerator initializes and returns a Generator.
func NewGenerator() *Generator {
	return &Generator{
		imports: map[string]string{
			pkgWriter:       "jwriter",
			pkgLexer:        "jlexer",
			"encoding/json": "json",
		},
		namer:         DefaultFieldNamer{},
		marshallers:   make(map[reflect.Type]bool),
		typesSeen:     make(map[reflect.Type]bool),
		functionNames: make(map[string]reflect.Type),
	}
}

// SetPkg sets the name and path of output package.
func (g *Generator) SetPkg(name, path string) {
	g.pkgName = name
	g.pkgPath = path
}

// SetBuildTags sets build tags for the output file.
func (g *Generator) SetBuildTags(tags string) {
	g.buildTags = tags
}

// SetFieldNamer sets field naming strategy.
func (g *Generator) SetFieldNamer(n FieldNamer) {
	g.namer = n
}

// UseSnakeCase sets snake_case field naming strategy.
func (g *Generator) UseSnakeCase() {
	g.namer = SnakeCaseFieldNamer{}
}

// OmitEmpty triggers `json=",omitempty"` behaviour by default.
func (g *Generator) OmitEmpty() {
	g.omitEmpty = true
}

// addTypes requests to generate en-/decoding functions for the given type.
func (g *Generator) addType(t reflect.Type) {
	if g.typesSeen[t] {
		return
	}
	for _, t1 := range g.typesUnseen {
		if t1 == t {
			return
		}
	}
	g.typesUnseen = append(g.typesUnseen, t)
}

// Add requests to generate (un-)marshallers and en-/decoding functions for the type of given object.
func (g *Generator) Add(obj interface{}) {
	g.addType(reflect.TypeOf(obj))
	g.marshallers[reflect.TypeOf(obj)] = true
}

// printHeader prints package declaration and imports.
func (g *Generator) printHeader() {
	if g.buildTags != "" {
		fmt.Println("// +build ", g.buildTags)
		fmt.Println()
	}
	fmt.Println("package ", g.pkgName)
	fmt.Println()

	byAlias := map[string]string{}
	var aliases []string
	for path, alias := range g.imports {
		aliases = append(aliases, alias)
		byAlias[alias] = path
	}

	sort.Strings(aliases)
	fmt.Println("import (")
	for _, alias := range g.imports {
		fmt.Printf("  %s %q\n", alias, byAlias[alias])
	}

	fmt.Println(")")
	fmt.Println("")
	fmt.Println("var _ = json.RawMessage{} // suppress unused package warning")

	fmt.Println()
}

// Run runs the generator and outputs generated code to out.
func (g *Generator) Run(out io.Writer) error {
	g.out = &bytes.Buffer{}

	for len(g.typesUnseen) > 0 {
		t := g.typesUnseen[len(g.typesUnseen)-1]
		g.typesUnseen = g.typesUnseen[:len(g.typesUnseen)-1]
		g.typesSeen[t] = true

		if err := g.genStructDecoder(t); err != nil {
			return err
		}
		if err := g.genStructEncoder(t); err != nil {
			return err
		}

		if !g.marshallers[t] {
			continue
		}

		if err := g.genStructMarshaller(t); err != nil {
			return err
		}
		if err := g.genStructUnmarshaller(t); err != nil {
			return err
		}
	}
	g.printHeader()
	_, err := out.Write(g.out.Bytes())
	return err
}

// pkgAlias creates and returns and import alias for a given package.
func (g *Generator) pkgAlias(pkgPath string) string {
	if alias := g.imports[pkgPath]; alias != "" {
		return alias
	}

	for i := 0; ; i++ {
		alias := path.Base(pkgPath)
		if i > 0 {
			alias += fmt.Sprint(i)
		}

		exists := false
		for _, v := range g.imports {
			if v == alias {
				exists = true
				break
			}
		}

		if !exists {
			g.imports[pkgPath] = alias
			return alias
		}
	}
}

// getType return the textual type name of given type that can be used in generated code.
func (g *Generator) getType(t reflect.Type) string {
	if t.Name() == "" || t.PkgPath() == "" {
		return t.String()
	} else if t.PkgPath() == g.pkgPath {
		return t.Name()
	}
	// TODO: unnamed structs.
	return g.pkgAlias(t.PkgPath()) + "." + t.Name()
}

// uniqueVarName returns a file-unique name that can be used for generated variables.
func (g *Generator) uniqueVarName() string {
	g.varCounter++
	return fmt.Sprint("v", g.varCounter)
}

// safeName escapes unsafe characters in pkg/type name and returns a string that can be used
// in encoder/decoder names for the type.
func safeName(t reflect.Type) string {
	name := t.PkgPath()
	if t.Name() == "" {
		name += "anonymous"
	} else {
		name += "." + t.Name()
	}

	var ret []rune
	for _, c := range name {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			ret = append(ret, c)
		} else {
			ret = append(ret, '_')
		}
	}
	return string(ret)
}

// functionName returns a function name for a given type with a given prefix. If a function
// with this prefix already exists for a type, it is returned.
//
// Method is used to track encoder/decoder names for the type.
func (g *Generator) functionName(prefix string, t reflect.Type) string {
	name := prefix + safeName(t)

	// Most of the names will be unique, try a shortcut first.
	if e, ok := g.functionNames[name]; !ok || e == t {
		g.functionNames[name] = t
		return name
	}

	// Search if the function already exists.
	for name1, t1 := range g.functionNames {
		if t1 == t && strings.HasPrefix(name1, prefix) {
			return name1
		}
	}

	// Create a new name in the case of a clash.
	for i := 1; ; i++ {
		nm := fmt.Sprint(name, i)
		if _, ok := g.functionNames[nm]; ok {
			continue
		}
		g.functionNames[nm] = t
		return nm
	}
}

// DefaultFieldsNamer implements trivial naming policy equivalent to encoding/json.
type DefaultFieldNamer struct{}

func (DefaultFieldNamer) GetJSONFieldName(t reflect.Type, f reflect.StructField) string {
	jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
	if jsonName != "" {
		return jsonName
	} else {
		return f.Name
	}
}

// SnakeCaseFieldNamer implements CamelCase to snake_case conversion for fields names.
type SnakeCaseFieldNamer struct{}

func camelToSnake(name string) string {
	var ret bytes.Buffer

	multipleUpper := false
	var lastUpper rune

	for _, c := range name {
		// Non-lowercase character after uppercase is considered to be uppercase too.
		isUpper := (unicode.IsUpper(c) || (lastUpper != 0 && !unicode.IsLower(c)))

		if lastUpper != 0 {
			// Output a delimiter if last character was either the first uppercase character
			// in a row, or the last one in a row (e.g. 'S' in "HTTPServer").
			// Do not output a delimiter at the beginning of the name.

			firstInRow := !multipleUpper
			lastInRow := !isUpper

			if ret.Len() > 0 && (firstInRow || lastInRow) {
				ret.WriteByte('_')
			}
			ret.WriteRune(unicode.ToLower(lastUpper))
		}

		// Buffer uppercase char, do not output it yet as a delimiter may be required if the
		// next character is lowercase.
		if isUpper {
			multipleUpper = (lastUpper != 0)
			lastUpper = c
			continue
		}

		ret.WriteRune(c)
		lastUpper = 0
		multipleUpper = false
	}

	if lastUpper != 0 {
		ret.WriteRune(unicode.ToLower(lastUpper))
	}
	return string(ret.Bytes())
}

func (SnakeCaseFieldNamer) GetJSONFieldName(t reflect.Type, f reflect.StructField) string {
	jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
	if jsonName != "" {
		return jsonName
	}

	return camelToSnake(f.Name)
}