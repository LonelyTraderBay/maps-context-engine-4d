package schemacontract

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type ProtoImport struct {
	FromPath    string    `json:"from_path"`
	Path        string    `json:"path"`
	Public      bool      `json:"public"`
	Unsupported bool      `json:"unsupported,omitempty"`
	Status      RefStatus `json:"status"`
}

type ProtoField struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Label     string          `json:"label,omitempty"`
	Number    int32           `json:"number"`
	Oneof     string          `json:"oneof,omitempty"`
	Options   string          `json:"options,omitempty"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type ProtoEnumValue struct {
	Name      string          `json:"name"`
	Number    int32           `json:"number"`
	Options   string          `json:"options,omitempty"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type ProtoMessage struct {
	Name      string          `json:"name"`
	FullName  string          `json:"full_name"`
	Fields    []ProtoField    `json:"fields"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type ProtoEnum struct {
	Name      string           `json:"name"`
	FullName  string           `json:"full_name"`
	Values    []ProtoEnumValue `json:"values"`
	SourceRef model.SourceRef  `json:"source_ref"`
}

type ProtoResult struct {
	FormatVersion uint16         `json:"format_version"`
	Identity      Identity       `json:"identity"`
	IdentityKey   model.Hash     `json:"identity_key"`
	Syntax        string         `json:"syntax,omitempty"`
	Coverage      Coverage       `json:"coverage"`
	Messages      []ProtoMessage `json:"messages"`
	Enums         []ProtoEnum    `json:"enums"`
	Imports       []ProtoImport  `json:"imports"`
	Diagnostics   []Diagnostic   `json:"diagnostics"`
}

type protoToken struct {
	text       string
	start, end int
	kind       byte
}

type protoParser struct {
	path        string
	document    SourceDocument
	tokens      []protoToken
	index       int
	packageName string
	result      *ProtoResult
}

func AnalyzeProto3(identity Identity, bundle Bundle) (ProtoResult, error) {
	result := ProtoResult{
		FormatVersion: 1,
		Identity:      identity,
		Coverage:      CoverageComplete,
		Messages:      []ProtoMessage{},
		Enums:         []ProtoEnum{},
		Imports:       []ProtoImport{},
		Diagnostics:   []Diagnostic{},
	}
	if identity.Protocol != ProtocolProto3 {
		return ProtoResult{}, fmt.Errorf("%w: Protocol Buffers analyzer requires protocol %q", ErrInvalidContract, ProtocolProto3)
	}
	documents, err := bundle.Validate(identity)
	if err != nil {
		return ProtoResult{}, err
	}
	identityKey, err := identity.Key()
	if err != nil {
		return ProtoResult{}, err
	}
	result.IdentityKey = identityKey

	parsed := make(map[string]bool)
	active := make(map[string]bool)
	sourceValues := documentValues(documents)
	tokenBudget := MaxFacts * 8
	var parseDocument func(string) error
	parseDocument = func(pathName string) error {
		if parsed[pathName] {
			return nil
		}
		active[pathName] = true
		parser, err := newProtoParser(pathName, documents[pathName], &result, tokenBudget)
		if err != nil {
			return err
		}
		tokenBudget -= len(parser.tokens)
		imports, err := parser.parseFile()
		if err != nil {
			return fmt.Errorf("%s: %v", pathName, err)
		}
		for _, imported := range imports {
			if len(result.Imports) >= MaxFacts {
				return fmt.Errorf("proto import count exceeds the supported bound")
			}
			if imported.Unsupported {
				result.Imports = append(result.Imports, ProtoImport{FromPath: pathName, Path: imported.Path, Public: imported.Public, Unsupported: true, Status: RefUnsupported})
				continue
			}
			resolved := ResolveBundleRef(sourceValues, pathName, imported.Path)
			importRecord := ProtoImport{FromPath: pathName, Path: imported.Path, Public: imported.Public, Status: resolved.Status}
			if resolved.Status == RefResolved {
				if active[resolved.Path] {
					importRecord.Status = RefCycle
					result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "proto_import_cycle", DocumentPath: pathName, Detail: "cyclic proto import remains unresolved"})
				} else if err := parseDocument(resolved.Path); err != nil {
					return err
				}
			} else {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{
					Code: "proto_import_" + string(resolved.Status), DocumentPath: pathName,
					Detail: "import remains unresolved outside the supplied allowlisted bundle",
				})
			}
			result.Imports = append(result.Imports, importRecord)
		}
		delete(active, pathName)
		parsed[pathName] = true
		return nil
	}
	if err := parseDocument(bundle.RootPath); err != nil {
		return ProtoResult{}, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	result.Syntax = "proto3"
	declarations := make(map[string]bool, len(result.Messages)+len(result.Enums))
	for _, message := range result.Messages {
		if declarations[message.FullName] {
			return ProtoResult{}, fmt.Errorf("%w: duplicate declaration %q", ErrInvalidContract, message.FullName)
		}
		declarations[message.FullName] = true
	}
	for _, enum := range result.Enums {
		if declarations[enum.FullName] {
			return ProtoResult{}, fmt.Errorf("%w: duplicate declaration %q", ErrInvalidContract, enum.FullName)
		}
		declarations[enum.FullName] = true
	}
	if len(result.Diagnostics) > 0 {
		result.Coverage = CoveragePartial
	}
	sort.Slice(result.Messages, func(i, j int) bool { return result.Messages[i].FullName < result.Messages[j].FullName })
	sort.Slice(result.Enums, func(i, j int) bool { return result.Enums[i].FullName < result.Enums[j].FullName })
	sort.Slice(result.Imports, func(i, j int) bool {
		if result.Imports[i].FromPath != result.Imports[j].FromPath {
			return result.Imports[i].FromPath < result.Imports[j].FromPath
		}
		return result.Imports[i].Path < result.Imports[j].Path
	})
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		if result.Diagnostics[i].DocumentPath != result.Diagnostics[j].DocumentPath {
			return result.Diagnostics[i].DocumentPath < result.Diagnostics[j].DocumentPath
		}
		if result.Diagnostics[i].Code != result.Diagnostics[j].Code {
			return result.Diagnostics[i].Code < result.Diagnostics[j].Code
		}
		return result.Diagnostics[i].Detail < result.Diagnostics[j].Detail
	})
	return result, nil
}

func documentValues(documents map[string]SourceDocument) map[string]any {
	values := make(map[string]any, len(documents))
	for pathName, document := range documents {
		values[pathName] = document.Bytes
	}
	return values
}

func newProtoParser(pathName string, document SourceDocument, result *ProtoResult, tokenBudget int) (*protoParser, error) {
	if !utf8.Valid(document.Bytes) {
		return nil, fmt.Errorf("source is not valid UTF-8")
	}
	tokens, err := lexProto(document.Bytes, tokenBudget)
	if err != nil {
		return nil, err
	}
	return &protoParser{path: pathName, document: document, tokens: tokens, result: result}, nil
}

func lexProto(source []byte, tokenBudget int) ([]protoToken, error) {
	tokens := make([]protoToken, 0, len(source)/4)
	for index := 0; index < len(source); {
		char := source[index]
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' || char == '\f' {
			index++
			continue
		}
		if char == '/' && index+1 < len(source) && source[index+1] == '/' {
			index += 2
			for index < len(source) && source[index] != '\n' {
				index++
			}
			continue
		}
		if char == '/' && index+1 < len(source) && source[index+1] == '*' {
			start := index
			index += 2
			for index+1 < len(source) && !(source[index] == '*' && source[index+1] == '/') {
				index++
			}
			if index+1 >= len(source) {
				return nil, fmt.Errorf("unterminated block comment at byte %d", start)
			}
			index += 2
			continue
		}
		start := index
		if char == '"' {
			quote := char
			index++
			escaped := false
			for index < len(source) {
				current := source[index]
				index++
				if escaped {
					escaped = false
					continue
				}
				if current == '\\' {
					escaped = true
					continue
				}
				if current == quote {
					break
				}
				if current == '\n' || current == '\r' {
					return nil, fmt.Errorf("newline in string literal at byte %d", start)
				}
			}
			if source[index-1] != quote {
				return nil, fmt.Errorf("unterminated string literal at byte %d", start)
			}
			unquoted, err := strconv.Unquote(string(source[start:index]))
			if err != nil {
				return nil, fmt.Errorf("invalid string literal at byte %d: %v", start, err)
			}
			tokens = append(tokens, protoToken{text: unquoted, start: start, end: index, kind: 's'})
			continue
		}
		if isProtoIdentifierStart(char) {
			size := 1
			index += size
			for index < len(source) {
				if !isProtoIdentifierPart(source[index]) {
					break
				}
				index++
			}
			tokens = append(tokens, protoToken{text: string(source[start:index]), start: start, end: index, kind: 'i'})
			continue
		}
		if char >= '0' && char <= '9' {
			index++
			for index < len(source) && source[index] >= '0' && source[index] <= '9' {
				index++
			}
			tokens = append(tokens, protoToken{text: string(source[start:index]), start: start, end: index, kind: 'n'})
			continue
		}
		if strings.ContainsRune("{}[]=;,.<>()+-", rune(char)) {
			index++
			tokens = append(tokens, protoToken{text: string(char), start: start, end: index, kind: 'p'})
			continue
		}
		return nil, fmt.Errorf("unsupported or malformed character %q at byte %d", char, index)
	}
	if len(tokens) > tokenBudget {
		return nil, fmt.Errorf("token count exceeds the supported bound")
	}
	return tokens, nil
}

func (parser *protoParser) parseFile() ([]ProtoImport, error) {
	if !parser.take("syntax") || !parser.take("=") {
		return nil, fmt.Errorf("first declaration must pin syntax to proto3")
	}
	syntax, ok := parser.nextKind('s')
	if !ok || syntax.text != "proto3" || !parser.take(";") {
		return nil, fmt.Errorf("only syntax = \"proto3\" is supported")
	}
	imports := []ProtoImport{}
	for !parser.done() {
		start := parser.current().start
		switch {
		case parser.take("package"):
			name, ok := parser.parseDottedName(false)
			if !ok || !parser.take(";") {
				return nil, fmt.Errorf("malformed package declaration at byte %d", start)
			}
			if parser.packageName != "" && parser.packageName != name {
				return nil, fmt.Errorf("conflicting package declarations")
			}
			parser.packageName = name
		case parser.take("import"):
			public := parser.take("public")
			weak := false
			if !public {
				weak = parser.take("weak")
			}
			name, ok := parser.nextKind('s')
			if !ok || !parser.take(";") {
				return nil, fmt.Errorf("malformed import declaration at byte %d", start)
			}
			if weak {
				parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_weak_import", DocumentPath: parser.path, Detail: "weak imports are not supported"})
				imports = append(imports, ProtoImport{Path: name.text, Unsupported: true})
				continue
			}
			if len(imports) >= MaxFacts {
				return nil, fmt.Errorf("proto import count exceeds the supported bound")
			}
			imports = append(imports, ProtoImport{Path: name.text, Public: public})
		case parser.take("message"):
			if err := parser.parseMessage(""); err != nil {
				return nil, err
			}
		case parser.take("enum"):
			if err := parser.parseEnum(""); err != nil {
				return nil, err
			}
		case parser.take("option"):
			if err := parser.skipStatement(); err != nil {
				return nil, err
			}
			parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_file_option", DocumentPath: parser.path, Detail: "file options are retained as a coverage gap"})
		case parser.take("service"):
			if err := parser.skipDeclaration(); err != nil {
				return nil, err
			}
			parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_service", DocumentPath: parser.path, Detail: "service and RPC declarations are outside this contract subset"})
		default:
			return nil, fmt.Errorf("unsupported top-level declaration %q at byte %d", parser.current().text, start)
		}
	}
	return imports, nil
}

func (parser *protoParser) parseMessage(parent string) error {
	start := parser.previous().start
	name, ok := parser.nextKind('i')
	if !ok || name.text == "message" || !parser.take("{") {
		return fmt.Errorf("malformed message declaration")
	}
	fullName := joinProtoName(parser.packageName, parent, name.text)
	message := ProtoMessage{Name: name.text, FullName: fullName, Fields: []ProtoField{}, SourceRef: parser.refRange(start, name.end)}
	fieldNumbers := make(map[int32]bool)
	fieldNames := make(map[string]bool)
	for !parser.done() && !parser.peek("}") {
		declarationStart := parser.current().start
		switch {
		case parser.take("message"):
			if err := parser.parseMessage(fullName); err != nil {
				return err
			}
		case parser.take("enum"):
			if err := parser.parseEnum(fullName); err != nil {
				return err
			}
		case parser.take("oneof"):
			oneofName, ok := parser.nextKind('i')
			if !ok || !parser.take("{") {
				return fmt.Errorf("malformed oneof declaration at byte %d", declarationStart)
			}
			for !parser.done() && !parser.peek("}") {
				if parser.take("option") {
					if err := parser.skipStatement(); err != nil {
						return err
					}
					parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_oneof_option", DocumentPath: parser.path, Detail: "oneof options are not interpreted"})
					continue
				}
				field, err := parser.parseField(oneofName.text)
				if err != nil {
					return err
				}
				if fieldNumbers[field.Number] || fieldNames[field.Name] {
					return fmt.Errorf("duplicate field tag or name %q in message %s", field.Name, fullName)
				}
				if len(message.Fields) >= MaxFacts {
					return fmt.Errorf("field count exceeds the supported bound in message %s", fullName)
				}
				fieldNumbers[field.Number], fieldNames[field.Name] = true, true
				message.Fields = append(message.Fields, field)
			}
			if !parser.take("}") {
				return fmt.Errorf("unterminated oneof in message %s", fullName)
			}
		case parser.take("option"), parser.take("reserved"), parser.take("extensions"):
			if err := parser.skipStatement(); err != nil {
				return err
			}
			parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_message_declaration", DocumentPath: parser.path, Detail: "message option/reservation declarations are retained as a coverage gap"})
		default:
			field, err := parser.parseField("")
			if err != nil {
				return err
			}
			if fieldNumbers[field.Number] || fieldNames[field.Name] {
				return fmt.Errorf("duplicate field tag or name %q in message %s", field.Name, fullName)
			}
			if len(message.Fields) >= MaxFacts {
				return fmt.Errorf("field count exceeds the supported bound in message %s", fullName)
			}
			fieldNumbers[field.Number], fieldNames[field.Name] = true, true
			message.Fields = append(message.Fields, field)
		}
	}
	close, ok := parser.takeToken("}")
	if !ok {
		return fmt.Errorf("unterminated message %s", fullName)
	}
	message.SourceRef = parser.refRange(start, close.end)
	if len(parser.result.Messages) >= MaxFacts {
		return fmt.Errorf("message count exceeds the supported bound")
	}
	parser.result.Messages = append(parser.result.Messages, message)
	return nil
}

func (parser *protoParser) parseField(oneof string) (ProtoField, error) {
	start := parser.current().start
	label := ""
	if parser.peek("optional") || parser.peek("repeated") || parser.peek("required") {
		label = parser.current().text
		parser.index++
		if label == "required" {
			return ProtoField{}, fmt.Errorf("required label is unsupported in proto3")
		}
		if oneof != "" {
			return ProtoField{}, fmt.Errorf("field labels are not allowed inside oneof")
		}
	}
	typeName, ok := parser.parseType()
	if !ok {
		return ProtoField{}, fmt.Errorf("malformed field type at byte %d", start)
	}
	name, ok := parser.nextKind('i')
	if !ok || !parser.take("=") {
		return ProtoField{}, fmt.Errorf("malformed field declaration at byte %d", start)
	}
	numberToken, ok := parser.nextKind('n')
	if !ok || (len(numberToken.text) > 1 && numberToken.text[0] == '0') {
		return ProtoField{}, fmt.Errorf("field number is not an integer at byte %d", start)
	}
	parsed, err := strconv.ParseInt(numberToken.text, 10, 32)
	if err != nil || parsed < 1 || parsed > 536870911 || (parsed >= 19000 && parsed <= 19999) {
		return ProtoField{}, fmt.Errorf("field number %q is outside the proto3 range or reserved range", numberToken.text)
	}
	options := ""
	if parser.take("[") {
		options, err = parser.skipBalancedOptions()
		if err != nil {
			return ProtoField{}, err
		}
	}
	end, ok := parser.takeToken(";")
	if !ok {
		return ProtoField{}, fmt.Errorf("field declaration is missing semicolon")
	}
	return ProtoField{Name: name.text, Type: typeName, Label: label, Number: int32(parsed), Oneof: oneof, Options: options, SourceRef: parser.refRange(start, end.end)}, nil
}

func (parser *protoParser) parseType() (string, bool) {
	if parser.peek("map") {
		parser.index++
		if !parser.take("<") {
			return "", false
		}
		key, ok := parser.parseType()
		if !ok || !validMapKey(key) || !parser.take(",") {
			return "", false
		}
		value, ok := parser.parseType()
		if !ok || !parser.take(">") {
			return "", false
		}
		return "map<" + key + "," + value + ">", true
	}
	leadingDot := parser.take(".")
	first, ok := parser.nextKind('i')
	if !ok {
		return "", false
	}
	name := first.text
	for parser.take(".") {
		segment, ok := parser.nextKind('i')
		if !ok {
			return "", false
		}
		name += "." + segment.text
	}
	if leadingDot {
		name = "." + name
	}
	return name, true
}

func (parser *protoParser) parseEnum(parent string) error {
	start := parser.previous().start
	name, ok := parser.nextKind('i')
	if !ok || !parser.take("{") {
		return fmt.Errorf("malformed enum declaration")
	}
	fullName := joinProtoName(parser.packageName, parent, name.text)
	enum := ProtoEnum{Name: name.text, FullName: fullName, Values: []ProtoEnumValue{}, SourceRef: parser.refRange(start, name.end)}
	allowAlias := false
	seenNames := make(map[string]bool)
	seenNumbers := make(map[int32]bool)
	for !parser.done() && !parser.peek("}") {
		declarationStart := parser.current().start
		if parser.take("option") {
			optionTokens, err := parser.takeStatementTokens()
			if err != nil {
				return err
			}
			if len(optionTokens) == 3 && optionTokens[0].text == "allow_alias" && optionTokens[1].text == "=" && optionTokens[2].text == "true" {
				allowAlias = true
			} else {
				parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_enum_option", DocumentPath: parser.path, Detail: "enum option is retained as a coverage gap"})
			}
			continue
		}
		if parser.take("reserved") {
			if err := parser.skipStatement(); err != nil {
				return err
			}
			parser.result.Diagnostics = append(parser.result.Diagnostics, Diagnostic{Code: "unsupported_enum_reservation", DocumentPath: parser.path, Detail: "enum reserved declarations are retained as a coverage gap"})
			continue
		}
		valueName, ok := parser.nextKind('i')
		if !ok || !parser.take("=") {
			return fmt.Errorf("malformed enum value at byte %d", declarationStart)
		}
		number, ok := parser.parseSignedInteger()
		if !ok || number < -2147483648 || number > 2147483647 {
			return fmt.Errorf("enum number outside int32 range")
		}
		options := ""
		var err error
		if parser.take("[") {
			options, err = parser.skipBalancedOptions()
			if err != nil {
				return err
			}
		}
		end, ok := parser.takeToken(";")
		if !ok || seenNames[valueName.text] || (seenNumbers[int32(number)] && !allowAlias) {
			return fmt.Errorf("duplicate or malformed enum value %q", valueName.text)
		}
		seenNames[valueName.text] = true
		seenNumbers[int32(number)] = true
		if len(enum.Values) >= MaxFacts {
			return fmt.Errorf("enum value count exceeds the supported bound in %s", fullName)
		}
		enum.Values = append(enum.Values, ProtoEnumValue{Name: valueName.text, Number: int32(number), Options: options, SourceRef: parser.refRange(declarationStart, end.end)})
	}
	close, ok := parser.takeToken("}")
	if !ok {
		return fmt.Errorf("unterminated enum %s", fullName)
	}
	if len(enum.Values) == 0 || enum.Values[0].Number != 0 {
		return fmt.Errorf("proto3 enum %s must begin with a zero-valued member", fullName)
	}
	enum.SourceRef = parser.refRange(start, close.end)
	if len(parser.result.Enums) >= MaxFacts {
		return fmt.Errorf("enum count exceeds the supported bound")
	}
	parser.result.Enums = append(parser.result.Enums, enum)
	return nil
}

func (parser *protoParser) parseDottedName(leadingDot bool) (string, bool) {
	dot := ""
	if leadingDot && parser.take(".") {
		dot = "."
	}
	first, ok := parser.nextKind('i')
	if !ok {
		return "", false
	}
	name := dot + first.text
	for parser.take(".") {
		segment, ok := parser.nextKind('i')
		if !ok {
			return "", false
		}
		name += "." + segment.text
	}
	return name, true
}

func (parser *protoParser) parseSignedInteger() (int64, bool) {
	sign := ""
	if parser.peek("-") {
		sign = "-"
		parser.index++
	}
	token, ok := parser.nextKind('n')
	if !ok || (len(token.text) > 1 && token.text[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseInt(sign+token.text, 10, 64)
	return value, err == nil
}

func (parser *protoParser) skipBalancedOptions() (string, error) {
	if parser.index == 0 {
		return "", fmt.Errorf("invalid option start")
	}
	start := parser.tokens[parser.index-1].start
	depth := 1
	for !parser.done() {
		token := parser.current()
		parser.index++
		if token.text == "[" {
			depth++
		} else if token.text == "]" {
			depth--
			if depth == 0 {
				return string(parser.document.Bytes[start:token.end]), nil
			}
		}
	}
	return "", fmt.Errorf("unterminated field options")
}

func (parser *protoParser) skipStatement() error {
	_, err := parser.takeStatementTokens()
	return err
}

func (parser *protoParser) takeStatementTokens() ([]protoToken, error) {
	tokens := []protoToken{}
	depth := 0
	for !parser.done() {
		token := parser.current()
		parser.index++
		if token.text == "[" || token.text == "(" {
			depth++
		} else if token.text == "]" || token.text == ")" {
			depth--
		}
		if token.text == ";" && depth == 0 {
			return tokens, nil
		}
		tokens = append(tokens, token)
	}
	return nil, fmt.Errorf("unterminated declaration; expected semicolon")
}

func (parser *protoParser) skipDeclaration() error {
	for !parser.done() {
		if parser.take(";") {
			return nil
		}
		if parser.take("{") {
			depth := 1
			for !parser.done() && depth > 0 {
				token := parser.current()
				parser.index++
				if token.text == "{" {
					depth++
				} else if token.text == "}" {
					depth--
				}
			}
			if depth != 0 {
				return fmt.Errorf("unterminated unsupported declaration")
			}
			return nil
		}
		parser.index++
	}
	return fmt.Errorf("unterminated unsupported declaration")
}

func (parser *protoParser) refRange(start, end int) model.SourceRef {
	ref := parser.document.Ref
	ref.Range = model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
	return ref
}

func (parser *protoParser) current() protoToken {
	if parser.done() {
		return protoToken{text: "<eof>", start: len(parser.document.Bytes), end: len(parser.document.Bytes)}
	}
	return parser.tokens[parser.index]
}

func (parser *protoParser) previous() protoToken {
	if parser.index == 0 {
		return protoToken{start: 0}
	}
	return parser.tokens[parser.index-1]
}

func (parser *protoParser) done() bool { return parser.index >= len(parser.tokens) }

func (parser *protoParser) peek(text string) bool {
	return !parser.done() && parser.current().text == text
}

func (parser *protoParser) take(text string) bool {
	if !parser.peek(text) {
		return false
	}
	parser.index++
	return true
}

func (parser *protoParser) takeToken(text string) (protoToken, bool) {
	if !parser.peek(text) {
		return protoToken{}, false
	}
	token := parser.current()
	parser.index++
	return token, true
}

func (parser *protoParser) nextKind(kind byte) (protoToken, bool) {
	if parser.done() || parser.current().kind != kind {
		return protoToken{}, false
	}
	token := parser.current()
	parser.index++
	return token, true
}

func joinProtoName(packageName, parent, name string) string {
	if parent != "" {
		return parent + "." + name
	}
	if packageName != "" {
		return packageName + "." + name
	}
	return name
}

func validMapKey(value string) bool {
	switch value {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64", "fixed32", "fixed64", "sfixed32", "sfixed64", "bool", "string":
		return true
	default:
		return false
	}
}

func isProtoIdentifierStart(value byte) bool {
	return value == '_' || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func isProtoIdentifierPart(value byte) bool {
	return isProtoIdentifierStart(value) || (value >= '0' && value <= '9')
}
