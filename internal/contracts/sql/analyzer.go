package sqlcontract

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	maxSQLSources             = 4096
	maxSQLSourceBytes         = 2 << 20
	maxSQLTotalBytes          = 64 << 20
	maxSQLStatementsPerSource = 100_000
	maxSQLTotalStatements     = 100_000
	maxSQLTokensPerStatement  = 100_000
	maxSQLTotalTokens         = 500_000
	maxSQLScanIssuesPerSource = 100_000
	maxSQLTotalDiagnostics    = 100_000
)

var (
	errSQLLexical    = errors.New("malformed SQL source")
	errSQLTokenLimit = errors.New("SQL statement token bound exceeded")
	errSQLIssueLimit = errors.New("SQL source diagnostic bound exceeded")
)

type sqlStatement struct {
	start      int
	end        int
	executable bool
}

type scanIssue struct {
	code       DiagnosticCode
	start, end int
}

type tokenKind uint8

const (
	tokenWord tokenKind = iota + 1
	tokenQuotedIdentifier
	tokenString
	tokenNumber
	tokenSymbol
)

type sqlToken struct {
	kind                  tokenKind
	text                  string
	start, end            int
	contentStart          int
	contentEnd            int
	identifierHasEscaping bool
}

type qualifiedName struct {
	schema, name string
	next         int
	start, end   int
	exact        bool
}

type pendingLineage struct {
	source     Source
	fromID     string
	schema     string
	table      string
	start, end int
	kind       RelationKind
}

type pendingColumnLineage struct {
	source     Source
	fromID     string
	schema     string
	table      string
	column     string
	start, end int
}

type pendingWildcardLineage struct {
	source     Source
	fromID     string
	schema     string
	table      string
	start, end int
}

type sqlAnalyzer struct {
	result          *Result
	tables          []Object
	lineage         []pendingLineage
	columnLineage   []pendingColumnLineage
	wildcardLineage []pendingWildcardLineage
	diagnosticIndex map[string]struct{}
	diagnosticLimit bool
}

// Analyze extracts only source-backed MySQL facts. It never opens paths,
// connects to a database, executes SQL, or resolves names outside the capture.
func Analyze(sources []Source, profile Profile) (Result, error) {
	if err := profile.Validate(); err != nil {
		return Result{}, err
	}
	if len(sources) == 0 || len(sources) > maxSQLSources {
		return Result{}, fmt.Errorf("%w: SQL source count is outside the supported bound", ErrInvalidContract)
	}

	ordered := append([]Source(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Ref.Path < ordered[j].Ref.Path })
	var totalBytes int64
	for i, source := range ordered {
		if i > 0 && ordered[i-1].Ref.Path == source.Ref.Path {
			return Result{}, fmt.Errorf("%w: duplicate captured source path", ErrInvalidContract)
		}
		if source.Role != RoleSchemaSource && source.Role != RoleMigrationSource {
			return Result{}, fmt.Errorf("%w: unknown SQL source role", ErrInvalidContract)
		}
		if len(source.Bytes) > maxSQLSourceBytes || int64(len(source.Bytes)) > maxSQLTotalBytes-totalBytes {
			return Result{}, fmt.Errorf("%w: SQL capture exceeds the source byte bound", ErrInvalidContract)
		}
		totalBytes += int64(len(source.Bytes))
		if source.Ref.Range.StartByte != 0 || source.Ref.Range.EndByte != int64(len(source.Bytes)) {
			return Result{}, fmt.Errorf("%w: SQL capture reference must cover the full source", ErrInvalidContract)
		}
		if err := source.Ref.ValidateAgainst(source.Bytes); err != nil {
			return Result{}, fmt.Errorf("%w: invalid captured SQL source: %v", ErrInvalidContract, err)
		}
	}

	result := Result{
		FormatVersion: ResultFormatVersion,
		Profile:       profile,
		Coverage:      CoverageComplete,
		Objects:       []Object{},
		Relations:     []Relation{},
		Diagnostics:   []Diagnostic{},
		Migrations:    []Migration{},
	}
	analyzer := sqlAnalyzer{result: &result, diagnosticIndex: make(map[string]struct{})}
	totalStatements, totalTokens := 0, 0
	for _, source := range ordered {
		if source.Role == RoleMigrationSource {
			result.Migrations = append(result.Migrations, Migration{
				Path: source.Ref.Path, Order: uint32(len(result.Migrations) + 1),
				AppliedState: MigrationStateUnknown, SourceRef: source.Ref,
			})
		}
		statements, issues, err := scanSQLStatements(source.Bytes)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrInvalidContract, err)
		}
		totalStatements += len(statements)
		if totalStatements > maxSQLTotalStatements {
			return Result{}, fmt.Errorf("%w: SQL capture statement bound exceeded", ErrInvalidContract)
		}
		for _, issue := range issues {
			analyzer.addDiagnostic(source, issue.code, issue.start, issue.end)
		}
		if analyzer.diagnosticLimit {
			return Result{}, fmt.Errorf("%w: SQL capture diagnostic bound exceeded", ErrInvalidContract)
		}
		for _, statement := range statements {
			tokens, err := tokenizeSQLStatement(source.Bytes, statement)
			if err != nil {
				if errors.Is(err, errSQLTokenLimit) {
					return Result{}, fmt.Errorf("%w: %v", ErrInvalidContract, err)
				}
				analyzer.addDiagnostic(source, DiagnosticMalformedSource, statement.start, statement.end)
				continue
			}
			if len(tokens) == 0 || statement.executable {
				continue
			}
			if len(tokens) > maxSQLTokensPerStatement {
				return Result{}, fmt.Errorf("%w: SQL statement token bound exceeded", ErrInvalidContract)
			}
			totalTokens += len(tokens)
			if totalTokens > maxSQLTotalTokens {
				return Result{}, fmt.Errorf("%w: SQL capture token bound exceeded", ErrInvalidContract)
			}
			analyzer.analyzeStatement(source, tokens)
			if analyzer.diagnosticLimit {
				return Result{}, fmt.Errorf("%w: SQL capture diagnostic bound exceeded", ErrInvalidContract)
			}
		}
	}
	analyzer.resolveLineage()
	if analyzer.diagnosticLimit {
		return Result{}, fmt.Errorf("%w: SQL capture diagnostic bound exceeded", ErrInvalidContract)
	}
	sort.Slice(result.Objects, func(i, j int) bool { return lessObject(result.Objects[i], result.Objects[j]) })
	sort.Slice(result.Relations, func(i, j int) bool { return lessRelation(result.Relations[i], result.Relations[j]) })
	sort.Slice(result.Diagnostics, func(i, j int) bool { return lessDiagnostic(result.Diagnostics[i], result.Diagnostics[j]) })
	if len(result.Diagnostics) != 0 {
		result.Coverage = CoveragePartial
	}
	if err := result.ValidateAgainst(ordered); err != nil {
		return Result{}, fmt.Errorf("%w: analyzer produced an invalid result: %v", ErrInvalidContract, err)
	}
	return result, nil
}

func (analyzer *sqlAnalyzer) analyzeStatement(source Source, tokens []sqlToken) {
	first, last := tokens[0], tokens[len(tokens)-1]
	dynamicTokens := dynamicSQLTokens(tokens)
	for _, token := range dynamicTokens {
		analyzer.addDiagnostic(source, DiagnosticDynamicSQL, token.start, token.end)
	}
	if len(dynamicTokens) != 0 {
		if keyword(tokens[0], "CREATE") {
			analyzer.parseCreateRoutine(source, tokens)
		}
		return
	}
	if !keyword(tokens[0], "CREATE") {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedStatement, first.start, last.end)
		return
	}
	if analyzer.parseCreateTable(source, tokens) ||
		analyzer.parseCreateView(source, tokens) ||
		analyzer.parseCreateTrigger(source, tokens) ||
		analyzer.parseCreateRoutine(source, tokens) {
		return
	}
	analyzer.addDiagnostic(source, DiagnosticUnsupportedStatement, first.start, last.end)
}

func (analyzer *sqlAnalyzer) parseCreateTable(source Source, tokens []sqlToken) bool {
	index := 1
	if index < len(tokens) && keyword(tokens[index], "TEMPORARY") {
		index++
	}
	if index >= len(tokens) || !keyword(tokens[index], "TABLE") {
		return false
	}
	index++
	if index+2 < len(tokens) && keyword(tokens[index], "IF") && keyword(tokens[index+1], "NOT") && keyword(tokens[index+2], "EXISTS") {
		index += 3
	}
	name, ok := parseQualifiedName(source.Bytes, tokens, index)
	if !ok {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[minInt(index, len(tokens)-1)].start, tokens[minInt(index, len(tokens)-1)].end)
		return true
	}
	index = name.next
	open := -1
	for i := index; i < len(tokens); i++ {
		if symbol(tokens[i], "(") {
			open = i
			break
		}
		if keyword(tokens[i], "ENGINE") || keyword(tokens[i], "AS") {
			break
		}
	}
	close := -1
	if open >= 0 {
		depth := 0
		for i := open; i < len(tokens); i++ {
			if symbol(tokens[i], "(") {
				depth++
			} else if symbol(tokens[i], ")") {
				depth--
				if depth == 0 {
					close = i
					break
				}
				if depth < 0 {
					break
				}
			}
		}
	}
	if close < 0 && open >= 0 {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[open].start, tokens[len(tokens)-1].end)
	}

	start := tokens[0].start
	end := tokens[len(tokens)-1].end
	tailStart := maxInt(close+1, index)
	engine, engineToken, engineOK := parseTableEngine(tokens, tailStart)
	if !engineOK {
		engine = &EngineIdentity{Class: EngineUnknown}
		if engineToken != nil {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, engineToken.start, engineToken.end)
		} else {
			analyzer.addDiagnostic(source, DiagnosticUnknownEngine, name.start, name.end)
		}
	} else if engine.Class == EngineUnsupported {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedEngine, engineToken.start, engineToken.end)
	}
	if unsupported := firstUnsupportedTableOption(tokens, tailStart); unsupported != nil {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, unsupported.start, unsupported.end)
	}
	table := analyzer.addObject(source, ObjectTable, name.schema, name.name, "", engine, start, end)
	if open < 0 {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, name.start, name.end)
		return true
	}
	if close < 0 {
		return true
	}
	if close == open+1 {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[open].start, tokens[close].end)
		return true
	}

	columnCount := 0
	for _, segment := range splitTableElements(tokens, open+1, close) {
		if len(segment) == 0 || isTableConstraint(segment[0]) {
			continue
		}
		columnName := segment[0]
		if !isIdentifier(columnName) || len(segment) < 2 {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, segment[0].start, segment[len(segment)-1].end)
			continue
		}
		if columnName.identifierHasEscaping {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, columnName.start, columnName.end)
			continue
		}
		column := analyzer.addObject(source, ObjectColumn, name.schema, columnName.text, table.ID, nil, columnName.start, segment[len(segment)-1].end)
		columnCount++
		evidenceStart, evidenceEnd, exact := identifierEvidence(source.Bytes, columnName)
		if !exact {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, columnName.start, columnName.end)
			continue
		}
		analyzer.result.Relations = append(analyzer.result.Relations, Relation{
			FromID: table.ID, ToID: column.ID, Kind: RelationTableHasColumn,
			SourceRef: sourceReference(source.Ref, evidenceStart, evidenceEnd),
		})
	}
	if columnCount == 0 {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[open].start, tokens[close].end)
	}
	return true
}

func (analyzer *sqlAnalyzer) parseCreateView(source Source, tokens []sqlToken) bool {
	viewIndex := findCreateObjectKeyword(tokens, "VIEW")
	if viewIndex < 0 {
		return false
	}
	name, ok := parseQualifiedName(source.Bytes, tokens, viewIndex+1)
	if !ok {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[viewIndex].start, tokens[viewIndex].end)
		return true
	}
	view := analyzer.addObject(source, ObjectView, name.schema, name.name, "", nil, tokens[0].start, tokens[len(tokens)-1].end)
	if name.schema == "" {
		analyzer.addDiagnostic(source, DiagnosticUnqualifiedReference, name.start, name.end)
		return true
	}
	asIndex := -1
	for i := name.next; i < len(tokens); i++ {
		if keyword(tokens[i], "AS") {
			asIndex = i
			break
		}
	}
	if asIndex < 0 || asIndex+1 >= len(tokens) || !keyword(tokens[asIndex+1], "SELECT") {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[name.next-1].start, tokens[len(tokens)-1].end)
		return true
	}
	selectIndex := asIndex + 1
	fromIndex := findTopLevelKeyword(tokens, selectIndex+1, "FROM")
	if fromIndex < 0 || fromIndex == selectIndex+1 || fromIndex+1 >= len(tokens) {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[selectIndex].start, tokens[len(tokens)-1].end)
		return true
	}
	projection := splitCommaSeparated(tokens, selectIndex+1, fromIndex)
	projectedColumns := make([]sqlToken, 0, len(projection))
	projectedWildcards := make([]sqlToken, 0, 1)
	for _, item := range projection {
		column, supported, isStar := parseProjectionColumn(item)
		if isStar {
			projectedWildcards = append(projectedWildcards, item[0])
			continue
		}
		if !supported {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, item[0].start, item[len(item)-1].end)
			continue
		}
		projectedColumns = append(projectedColumns, column)
	}
	tableName, ok := parseQualifiedName(source.Bytes, tokens, fromIndex+1)
	if !ok {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[fromIndex].start, tokens[len(tokens)-1].end)
		return true
	}
	if tableName.schema == "" {
		analyzer.addDiagnostic(source, DiagnosticUnqualifiedReference, tableName.start, tableName.end)
		return true
	}
	if !tableName.exact {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tableName.start, tableName.end)
		return true
	}
	additionalSources := hasAdditionalTableSources(tokens, tableName.next)
	if additionalSources {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[tableName.next].start, tokens[len(tokens)-1].end)
	}
	start, end := tableName.start, tableName.end
	analyzer.lineage = append(analyzer.lineage, pendingLineage{
		source: source, fromID: view.ID, schema: tableName.schema, table: tableName.name,
		start: start, end: end, kind: RelationViewReadsTable,
	})
	if !additionalSources {
		for _, wildcard := range projectedWildcards {
			analyzer.wildcardLineage = append(analyzer.wildcardLineage, pendingWildcardLineage{
				source: source, fromID: view.ID, schema: tableName.schema, table: tableName.name,
				start: wildcard.start, end: wildcard.end,
			})
		}
		for _, column := range projectedColumns {
			columnStart, columnEnd, exact := identifierEvidence(source.Bytes, column)
			if !exact {
				analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, column.start, column.end)
				continue
			}
			analyzer.columnLineage = append(analyzer.columnLineage, pendingColumnLineage{
				source: source, fromID: view.ID, schema: tableName.schema, table: tableName.name,
				column: column.text, start: columnStart, end: columnEnd,
			})
		}
	}
	return true
}

func (analyzer *sqlAnalyzer) parseCreateTrigger(source Source, tokens []sqlToken) bool {
	triggerIndex := findCreateObjectKeyword(tokens, "TRIGGER")
	if triggerIndex < 0 {
		return false
	}
	name, ok := parseQualifiedName(source.Bytes, tokens, triggerIndex+1)
	if !ok {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[triggerIndex].start, tokens[triggerIndex].end)
		return true
	}
	trigger := analyzer.addObject(source, ObjectTrigger, name.schema, name.name, "", nil, tokens[0].start, tokens[len(tokens)-1].end)
	if name.schema == "" {
		analyzer.addDiagnostic(source, DiagnosticUnqualifiedReference, name.start, name.end)
		return true
	}
	onIndex := findTopLevelKeyword(tokens, name.next, "ON")
	if onIndex < 0 || onIndex+1 >= len(tokens) {
		analyzer.addDiagnostic(source, DiagnosticUnresolvedReference, name.start, name.end)
		return true
	}
	tableName, ok := parseQualifiedName(source.Bytes, tokens, onIndex+1)
	if !ok {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[onIndex].start, tokens[len(tokens)-1].end)
		return true
	}
	if tableName.schema == "" {
		analyzer.addDiagnostic(source, DiagnosticUnqualifiedReference, tableName.start, tableName.end)
	} else if !tableName.exact {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tableName.start, tableName.end)
	}
	rowIndex := tableName.next + 2
	validRowClause := tableName.next >= 0 && rowIndex < len(tokens) && keyword(tokens[tableName.next], "FOR") &&
		keyword(tokens[tableName.next+1], "EACH") && keyword(tokens[rowIndex], "ROW") && rowIndex+1 < len(tokens)
	validEvent := onIndex == name.next+2 && (keyword(tokens[name.next], "BEFORE") || keyword(tokens[name.next], "AFTER")) &&
		(keyword(tokens[name.next+1], "INSERT") || keyword(tokens[name.next+1], "UPDATE") || keyword(tokens[name.next+1], "DELETE"))
	validHeader := validEvent && validRowClause
	if !validEvent || !validHeader {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, name.start, tableName.end)
	} else if tableName.schema != "" && tableName.exact {
		analyzer.lineage = append(analyzer.lineage, pendingLineage{
			source: source, fromID: trigger.ID, schema: tableName.schema, table: tableName.name,
			start: tableName.start, end: tableName.end, kind: RelationTriggerOnTable,
		})
	}
	if validRowClause {
		analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[rowIndex+1].start, tokens[len(tokens)-1].end)
	}
	return true
}

func (analyzer *sqlAnalyzer) parseCreateRoutine(source Source, tokens []sqlToken) bool {
	for _, kind := range []struct {
		keyword string
		object  ObjectKind
	}{
		{keyword: "PROCEDURE", object: ObjectProcedure},
		{keyword: "FUNCTION", object: ObjectFunction},
		{keyword: "EVENT", object: ObjectEvent},
	} {
		index := findCreateObjectKeyword(tokens, kind.keyword)
		if index < 0 {
			continue
		}
		name, ok := parseQualifiedName(source.Bytes, tokens, index+1)
		if !ok {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[index].start, tokens[index].end)
			return true
		}
		analyzer.addObject(source, kind.object, name.schema, name.name, "", nil, tokens[0].start, tokens[len(tokens)-1].end)
		if name.next < len(tokens) {
			analyzer.addDiagnostic(source, DiagnosticUnsupportedSyntax, tokens[name.next].start, tokens[len(tokens)-1].end)
		}
		return true
	}
	return false
}

func (analyzer *sqlAnalyzer) addObject(source Source, kind ObjectKind, schema, name, parentID string, engine *EngineIdentity, start, end int) Object {
	ref := sourceReference(source.Ref, start, end)
	object := Object{
		ID: ObjectIDFor(kind, ref), Kind: kind, Schema: schema, Name: name,
		ParentID: parentID, Engine: engine, SourceRef: ref,
	}
	analyzer.result.Objects = append(analyzer.result.Objects, object)
	if kind == ObjectTable {
		analyzer.tables = append(analyzer.tables, object)
	}
	return object
}

func (analyzer *sqlAnalyzer) addDiagnostic(source Source, code DiagnosticCode, start, end int) {
	if analyzer.diagnosticLimit {
		return
	}
	if start < 0 || end < start || end > len(source.Bytes) {
		return
	}
	key := string(code) + "\x00" + source.Ref.Path + "\x00" + fmt.Sprint(start)
	if _, exists := analyzer.diagnosticIndex[key]; exists {
		return
	}
	if len(analyzer.result.Diagnostics) >= maxSQLTotalDiagnostics {
		analyzer.diagnosticLimit = true
		return
	}
	analyzer.diagnosticIndex[key] = struct{}{}
	analyzer.result.Diagnostics = append(analyzer.result.Diagnostics, Diagnostic{
		Code: code, SourceRef: sourceReference(source.Ref, start, end),
	})
}

func (analyzer *sqlAnalyzer) resolveLineage() {
	tablesByName := make(map[string][]Object, len(analyzer.tables))
	columnsByTable := make(map[string][]Object)
	for _, object := range analyzer.result.Objects {
		if object.Kind == ObjectTable {
			key := object.Schema + "\x00" + object.Name
			tablesByName[key] = append(tablesByName[key], object)
		}
		if object.Kind == ObjectColumn {
			columnsByTable[object.ParentID] = append(columnsByTable[object.ParentID], object)
		}
	}
	resolvedTables := make(map[string]Object)
	resolveTable := func(source Source, schema, name string, start, end int) (Object, bool) {
		nameKey := schema + "\x00" + name
		key := snapshotObjectNameKey(source.Ref, schema, name)
		if table, ok := resolvedTables[key]; ok {
			return table, true
		}
		matches := tablesByName[nameKey]
		snapshotMatches := make([]Object, 0, len(matches))
		for _, match := range matches {
			if sameSnapshot(source.Ref, match.SourceRef) {
				snapshotMatches = append(snapshotMatches, match)
			}
		}
		if len(snapshotMatches) == 0 {
			analyzer.addDiagnostic(source, DiagnosticUnresolvedReference, start, end)
			return Object{}, false
		}
		if len(snapshotMatches) != 1 {
			analyzer.addDiagnostic(source, DiagnosticAmbiguousReference, start, end)
			return Object{}, false
		}
		resolvedTables[key] = snapshotMatches[0]
		return snapshotMatches[0], true
	}
	relationKeys := make(map[string]struct{})
	for _, pending := range analyzer.lineage {
		table, ok := resolveTable(pending.source, pending.schema, pending.table, pending.start, pending.end)
		if !ok {
			continue
		}
		key := string(pending.kind) + "\x00" + pending.fromID + "\x00" + table.ID
		if _, exists := relationKeys[key]; exists {
			continue
		}
		relationKeys[key] = struct{}{}
		analyzer.result.Relations = append(analyzer.result.Relations, Relation{
			FromID: pending.fromID, ToID: table.ID, Kind: pending.kind,
			SourceRef: sourceReference(pending.source.Ref, pending.start, pending.end),
		})
	}
	for _, pending := range analyzer.columnLineage {
		table, ok := resolveTable(pending.source, pending.schema, pending.table, pending.start, pending.end)
		if !ok {
			continue
		}
		var matches []Object
		for _, column := range columnsByTable[table.ID] {
			if column.Name == pending.column {
				matches = append(matches, column)
			}
		}
		if len(matches) == 0 {
			analyzer.addDiagnostic(pending.source, DiagnosticUnresolvedReference, pending.start, pending.end)
			continue
		}
		if len(matches) != 1 {
			analyzer.addDiagnostic(pending.source, DiagnosticAmbiguousReference, pending.start, pending.end)
			continue
		}
		key := string(RelationViewReadsColumn) + "\x00" + pending.fromID + "\x00" + matches[0].ID
		if _, exists := relationKeys[key]; exists {
			continue
		}
		relationKeys[key] = struct{}{}
		analyzer.result.Relations = append(analyzer.result.Relations, Relation{
			FromID: pending.fromID, ToID: matches[0].ID, Kind: RelationViewReadsColumn,
			SourceRef: sourceReference(pending.source.Ref, pending.start, pending.end),
		})
	}
	for _, pending := range analyzer.wildcardLineage {
		table, ok := resolveTable(pending.source, pending.schema, pending.table, pending.start, pending.end)
		if !ok {
			continue
		}
		columns := columnsByTable[table.ID]
		if len(columns) == 0 {
			analyzer.addDiagnostic(pending.source, DiagnosticUnresolvedReference, pending.start, pending.end)
			continue
		}
		for _, column := range columns {
			key := string(RelationViewReadsColumn) + "\x00" + pending.fromID + "\x00" + column.ID
			if _, exists := relationKeys[key]; exists {
				continue
			}
			relationKeys[key] = struct{}{}
			analyzer.result.Relations = append(analyzer.result.Relations, Relation{
				FromID: pending.fromID, ToID: column.ID, Kind: RelationViewReadsColumn,
				SourceRef: sourceReference(pending.source.Ref, pending.start, pending.end),
			})
		}
	}
}

func scanSQLStatements(source []byte) ([]sqlStatement, []scanIssue, error) {
	statements := make([]sqlStatement, 0)
	issues := make([]scanIssue, 0)
	var err error
	delimiter := []byte(";")
	statementStart := 0
	hasContent := false
	executable := false
	quote := byte(0)
	quoteStart := -1
	lineComment := false
	blockComment := false
	blockStart := -1
	blockExecutable := false
	for index := 0; index < len(source); {
		if quote == 0 && !lineComment && !blockComment && !hasContent && (index == 0 || source[index-1] == '\n') {
			if recognized, next, nextDelimiter, issue := parseDelimiterDirective(source, index); recognized {
				if issue != nil {
					issues, err = appendSQLScanIssue(issues, *issue)
					if err != nil {
						return nil, nil, err
					}
				} else {
					delimiter = nextDelimiter
				}
				statementStart = next
				hasContent = false
				executable = false
				index = next
				continue
			}
		}
		character := source[index]
		if lineComment {
			if character == '\n' {
				lineComment = false
			}
			index++
			continue
		}
		if blockComment {
			if character == '*' && index+1 < len(source) && source[index+1] == '/' {
				index += 2
				blockComment = false
				if blockExecutable {
					issues, err = appendSQLScanIssue(issues, scanIssue{code: DiagnosticExecutableComment, start: blockStart, end: index})
					if err != nil {
						return nil, nil, err
					}
					executable = true
					hasContent = true
				}
				continue
			}
			index++
			continue
		}
		if quote != 0 {
			if quote != '`' && character == '\\' {
				// SQL_MODE changes whether backslashes escape quotes. Without a
				// captured mode, the remainder cannot be split safely.
				issues, err = appendSQLScanIssue(issues, scanIssue{code: DiagnosticUnsupportedSyntax, start: quoteStart, end: len(source)})
				if err != nil {
					return nil, nil, err
				}
				return statements, issues, nil
			}
			if character == quote {
				if index+1 < len(source) && source[index+1] == quote {
					index += 2
					continue
				}
				quote = 0
			}
			index++
			continue
		}
		if character == '\'' || character == '"' || character == '`' {
			quote = character
			quoteStart = index
			hasContent = true
			index++
			continue
		}
		if character == '/' && index+1 < len(source) && source[index+1] == '*' {
			blockStart = index
			blockExecutable = index+2 < len(source) && source[index+2] == '!'
			blockComment = true
			index += 2
			continue
		}
		if character == '#' || (character == '-' && index+2 < len(source) && source[index+1] == '-' && (source[index+2] <= ' ' || source[index+2] == 0x7f)) {
			lineComment = true
			index++
			if character == '-' {
				index++
			}
			continue
		}
		if bytes.HasPrefix(source[index:], delimiter) {
			if hasContent {
				statements = append(statements, sqlStatement{start: statementStart, end: index, executable: executable})
				if len(statements) > maxSQLStatementsPerSource {
					return nil, nil, errors.New("SQL statement count bound exceeded")
				}
			}
			index += len(delimiter)
			statementStart = index
			hasContent = false
			executable = false
			continue
		}
		if !isSQLWhitespace(character) {
			hasContent = true
		}
		index++
	}
	if quote != 0 {
		issues, err = appendSQLScanIssue(issues, scanIssue{code: DiagnosticMalformedSource, start: quoteStart, end: len(source)})
		if err != nil {
			return nil, nil, err
		}
	} else if blockComment {
		issues, err = appendSQLScanIssue(issues, scanIssue{code: DiagnosticMalformedSource, start: blockStart, end: len(source)})
		if err != nil {
			return nil, nil, err
		}
	} else if hasContent {
		statements = append(statements, sqlStatement{start: statementStart, end: len(source), executable: executable})
	}
	if len(statements) > maxSQLStatementsPerSource {
		return nil, nil, errors.New("SQL statement count bound exceeded")
	}
	return statements, issues, nil
}

func appendSQLScanIssue(issues []scanIssue, issue scanIssue) ([]scanIssue, error) {
	if len(issues) >= maxSQLScanIssuesPerSource {
		return nil, errSQLIssueLimit
	}
	return append(issues, issue), nil
}

func parseDelimiterDirective(source []byte, lineStart int) (bool, int, []byte, *scanIssue) {
	index := lineStart
	for index < len(source) && (source[index] == ' ' || source[index] == '\t') {
		index++
	}
	const command = "DELIMITER"
	if len(source)-index < len(command) || !strings.EqualFold(string(source[index:index+len(command)]), command) {
		return false, lineStart, nil, nil
	}
	commandEnd := index + len(command)
	if commandEnd < len(source) && source[commandEnd] != ' ' && source[commandEnd] != '\t' {
		return false, lineStart, nil, nil
	}
	lineEnd := commandEnd
	for lineEnd < len(source) && source[lineEnd] != '\n' {
		lineEnd++
	}
	next := lineEnd
	if next < len(source) {
		next++
	}
	for commandEnd < lineEnd && (source[commandEnd] == ' ' || source[commandEnd] == '\t') {
		commandEnd++
	}
	delimiterStart := commandEnd
	for commandEnd < lineEnd && source[commandEnd] != ' ' && source[commandEnd] != '\t' && source[commandEnd] != '\r' {
		commandEnd++
	}
	delimiter := source[delimiterStart:commandEnd]
	valid := len(delimiter) > 0 && len(delimiter) <= 16
	for _, character := range delimiter {
		if isSQLWhitespace(character) || isSQLIdentifierByte(character) && character != '$' ||
			character == '\'' || character == '"' || character == '`' || character == '\\' {
			valid = false
		}
	}
	trailing := bytes.TrimSpace(source[commandEnd:lineEnd])
	if len(trailing) > 0 && !bytes.HasPrefix(trailing, []byte("--")) && !bytes.HasPrefix(trailing, []byte("#")) {
		valid = false
	}
	if !valid {
		issue := &scanIssue{code: DiagnosticMalformedSource, start: index, end: lineEnd}
		return true, next, nil, issue
	}
	return true, next, append([]byte(nil), delimiter...), nil
}

func tokenizeSQLStatement(source []byte, statement sqlStatement) ([]sqlToken, error) {
	tokens := make([]sqlToken, 0, 32)
	for index := statement.start; index < statement.end; {
		character := source[index]
		if isSQLWhitespace(character) {
			index++
			continue
		}
		if character == '#' || (character == '-' && index+2 < statement.end && source[index+1] == '-' && (source[index+2] <= ' ' || source[index+2] == 0x7f)) {
			for index < statement.end && source[index] != '\n' {
				index++
			}
			continue
		}
		if character == '/' && index+1 < statement.end && source[index+1] == '*' {
			end := bytes.Index(source[index+2:statement.end], []byte("*/"))
			if end < 0 {
				return nil, errSQLLexical
			}
			index += end + 4
			continue
		}
		if character == '\'' || character == '"' || character == '`' {
			start := index
			quote := character
			index++
			contentStart := index
			var value strings.Builder
			escaped := false
			closed := false
			for index < statement.end {
				current := source[index]
				if quote != '`' && current == '\\' {
					escaped = true
					if index+1 >= statement.end {
						break
					}
					value.WriteByte(source[index+1])
					index += 2
					continue
				}
				if current == quote {
					if index+1 < statement.end && source[index+1] == quote {
						escaped = true
						value.WriteByte(quote)
						index += 2
						continue
					}
					contentEnd := index
					index++
					kind := tokenString
					if quote == '`' {
						kind = tokenQuotedIdentifier
					}
					tokens = append(tokens, sqlToken{kind: kind, text: value.String(), start: start, end: index, contentStart: contentStart, contentEnd: contentEnd, identifierHasEscaping: escaped})
					closed = true
					break
				}
				value.WriteByte(current)
				index++
			}
			if !closed {
				return nil, errSQLLexical
			}
			continue
		}
		runeValue, runeSize := utf8.DecodeRune(source[index:statement.end])
		if runeValue == utf8.RuneError && runeSize == 1 {
			return nil, errSQLLexical
		}
		if isIdentifierStart(runeValue) {
			start := index
			index += runeSize
			for index < statement.end {
				nextRune, nextSize := utf8.DecodeRune(source[index:statement.end])
				if !isIdentifierPart(nextRune) {
					break
				}
				index += nextSize
			}
			tokens = append(tokens, sqlToken{kind: tokenWord, text: string(source[start:index]), start: start, end: index, contentStart: start, contentEnd: index})
		} else if unicode.IsDigit(runeValue) {
			start := index
			index += runeSize
			for index < statement.end {
				nextRune, nextSize := utf8.DecodeRune(source[index:statement.end])
				if !unicode.IsDigit(nextRune) && nextRune != '.' {
					break
				}
				index += nextSize
			}
			tokens = append(tokens, sqlToken{kind: tokenNumber, text: string(source[start:index]), start: start, end: index})
		} else {
			start := index
			operator := ""
			for _, candidate := range []string{"->>", "<=>", "::", "||", "&&", ":=", "->"} {
				if bytes.HasPrefix(source[index:statement.end], []byte(candidate)) {
					operator = candidate
					break
				}
			}
			if operator != "" {
				index += len(operator)
			} else {
				index += runeSize
			}
			tokens = append(tokens, sqlToken{kind: tokenSymbol, text: string(source[start:index]), start: start, end: index})
		}
		if len(tokens) > maxSQLTokensPerStatement {
			return nil, errSQLTokenLimit
		}
	}
	return tokens, nil
}

func parseQualifiedName(source []byte, tokens []sqlToken, index int) (qualifiedName, bool) {
	if index >= len(tokens) || !isIdentifier(tokens[index]) || tokens[index].identifierHasEscaping {
		return qualifiedName{}, false
	}
	first := tokens[index]
	result := qualifiedName{name: first.text, next: index + 1, start: first.start, end: first.end, exact: exactIdentifierBytes(source, first)}
	if result.next+1 < len(tokens) && symbol(tokens[result.next], ".") && isIdentifier(tokens[result.next+1]) && !tokens[result.next+1].identifierHasEscaping {
		second := tokens[result.next+1]
		result.schema = first.text
		result.name = second.text
		result.end = second.end
		result.next += 2
		result.exact = result.exact && exactIdentifierBytes(source, second)
	}
	return result, true
}

func parseTableEngine(tokens []sqlToken, start int) (*EngineIdentity, *sqlToken, bool) {
	var engineToken *sqlToken
	for i := maxInt(start, 0); i < len(tokens); i++ {
		if !keyword(tokens[i], "ENGINE") {
			continue
		}
		if engineToken != nil {
			return nil, &tokens[i], false
		}
		engineToken = &tokens[i]
		valueIndex := i + 1
		if valueIndex < len(tokens) && symbol(tokens[valueIndex], "=") {
			valueIndex++
		}
		if valueIndex >= len(tokens) || (tokens[valueIndex].kind != tokenWord && tokens[valueIndex].kind != tokenQuotedIdentifier) {
			return nil, engineToken, false
		}
		name := tokens[valueIndex].text
		switch {
		case strings.EqualFold(name, "InnoDB"):
			return &EngineIdentity{Class: EngineInnoDB, DeclaredName: name}, engineToken, true
		case strings.EqualFold(name, "NDBCLUSTER"):
			return &EngineIdentity{Class: EngineNDBCluster, DeclaredName: name}, engineToken, true
		default:
			return &EngineIdentity{Class: EngineUnsupported, DeclaredName: name}, engineToken, true
		}
	}
	return nil, nil, false
}

func firstUnsupportedTableOption(tokens []sqlToken, start int) *sqlToken {
	for index := maxInt(start, 0); index < len(tokens); {
		option := tokens[index]
		switch {
		case keyword(option, "ENGINE"):
			index++
			if index < len(tokens) && symbol(tokens[index], "=") {
				index++
			}
			if index >= len(tokens) {
				return &option
			}
			index++
		case keyword(option, "DEFAULT"):
			index++
			if index < len(tokens) && (keyword(tokens[index], "CHARSET") || keyword(tokens[index], "COLLATE")) {
				index++
			} else if index+1 < len(tokens) && keyword(tokens[index], "CHARACTER") && keyword(tokens[index+1], "SET") {
				index += 2
			} else {
				return &option
			}
			if index < len(tokens) && symbol(tokens[index], "=") {
				index++
			}
			if index >= len(tokens) || !isIdentifier(tokens[index]) {
				return &option
			}
			index++
		case keyword(option, "CHARSET") || keyword(option, "COLLATE"):
			index++
			if index < len(tokens) && symbol(tokens[index], "=") {
				index++
			}
			if index >= len(tokens) || !isIdentifier(tokens[index]) {
				return &option
			}
			index++
		case keyword(option, "CHARACTER") && index+1 < len(tokens) && keyword(tokens[index+1], "SET"):
			index += 2
			if index < len(tokens) && symbol(tokens[index], "=") {
				index++
			}
			if index >= len(tokens) || !isIdentifier(tokens[index]) {
				return &option
			}
			index++
		case keyword(option, "AUTO_INCREMENT") || keyword(option, "AVG_ROW_LENGTH") ||
			keyword(option, "MAX_ROWS") || keyword(option, "MIN_ROWS") || keyword(option, "ROW_FORMAT") ||
			keyword(option, "COMMENT") || keyword(option, "KEY_BLOCK_SIZE") || keyword(option, "CHECKSUM") ||
			keyword(option, "DELAY_KEY_WRITE") || keyword(option, "PACK_KEYS"):
			index++
			if index < len(tokens) && symbol(tokens[index], "=") {
				index++
			}
			if index >= len(tokens) || tokens[index].kind == tokenSymbol {
				return &option
			}
			index++
		default:
			return &option
		}
	}
	return nil
}

func splitTableElements(tokens []sqlToken, start, end int) [][]sqlToken {
	segments := make([][]sqlToken, 0)
	depth, segmentStart := 0, start
	for i := start; i < end; i++ {
		if symbol(tokens[i], "(") {
			depth++
		} else if symbol(tokens[i], ")") {
			depth--
		} else if symbol(tokens[i], ",") && depth == 0 {
			segments = append(segments, tokens[segmentStart:i])
			segmentStart = i + 1
		}
	}
	if segmentStart < end {
		segments = append(segments, tokens[segmentStart:end])
	}
	return segments
}

func splitCommaSeparated(tokens []sqlToken, start, end int) [][]sqlToken {
	return splitTableElements(tokens, start, end)
}

func parseProjectionColumn(item []sqlToken) (sqlToken, bool, bool) {
	if len(item) == 1 && symbol(item[0], "*") {
		return sqlToken{}, true, true
	}
	if len(item) == 1 && isIdentifier(item[0]) {
		return item[0], true, false
	}
	if len(item) == 3 && isIdentifier(item[0]) && keyword(item[1], "AS") && isIdentifier(item[2]) {
		return item[0], true, false
	}
	return sqlToken{}, false, false
}

func findCreateObjectKeyword(tokens []sqlToken, name string) int {
	if len(tokens) < 3 || !keyword(tokens[0], "CREATE") {
		return -1
	}
	limit := minInt(len(tokens), 20)
	for i := 1; i < limit; i++ {
		if keyword(tokens[i], "SELECT") || keyword(tokens[i], "AS") {
			return -1
		}
		if keyword(tokens[i], name) {
			return i
		}
	}
	return -1
}

func findTopLevelKeyword(tokens []sqlToken, start int, word string) int {
	depth := 0
	for i := start; i < len(tokens); i++ {
		if symbol(tokens[i], "(") {
			depth++
		} else if symbol(tokens[i], ")") {
			depth--
		} else if depth == 0 && keyword(tokens[i], word) {
			return i
		}
	}
	return -1
}

func hasAdditionalTableSources(tokens []sqlToken, start int) bool {
	depth := 0
	for i := start; i < len(tokens); i++ {
		if symbol(tokens[i], "(") {
			depth++
			continue
		}
		if symbol(tokens[i], ")") {
			depth--
			continue
		}
		if keyword(tokens[i], "JOIN") || keyword(tokens[i], "SELECT") || keyword(tokens[i], "FROM") {
			return true
		}
		if depth != 0 {
			continue
		}
		if symbol(tokens[i], ",") || keyword(tokens[i], "LEFT") ||
			keyword(tokens[i], "RIGHT") || keyword(tokens[i], "INNER") || keyword(tokens[i], "CROSS") ||
			keyword(tokens[i], "NATURAL") || keyword(tokens[i], "UNION") {
			return true
		}
	}
	return false
}

func isTableConstraint(token sqlToken) bool {
	return keyword(token, "PRIMARY") || keyword(token, "UNIQUE") || keyword(token, "KEY") ||
		keyword(token, "INDEX") || keyword(token, "CONSTRAINT") || keyword(token, "FOREIGN") ||
		keyword(token, "CHECK") || keyword(token, "FULLTEXT") || keyword(token, "SPATIAL")
}

func dynamicSQLTokens(tokens []sqlToken) []sqlToken {
	hasPrepareOrExecute := false
	for _, token := range tokens {
		if keyword(token, "PREPARE") || keyword(token, "EXECUTE") {
			hasPrepareOrExecute = true
			break
		}
	}
	if !hasPrepareOrExecute {
		return nil
	}
	dynamic := make([]sqlToken, 0, 4)
	for _, token := range tokens {
		if keyword(token, "PREPARE") || keyword(token, "EXECUTE") || keyword(token, "CONCAT") ||
			(token.kind == tokenSymbol && (token.text == "||" || token.text == ":=")) {
			dynamic = append(dynamic, token)
		}
	}
	return dynamic
}

func identifierEvidence(source []byte, token sqlToken) (int, int, bool) {
	if !isIdentifier(token) || token.identifierHasEscaping || token.contentStart < 0 || token.contentEnd < token.contentStart {
		return 0, 0, false
	}
	start, end := token.contentStart, token.contentEnd
	if end > len(source) || start > end {
		return 0, 0, false
	}
	return start, end, string(source[start:end]) == token.text
}

func exactIdentifierBytes(source []byte, token sqlToken) bool {
	if !isIdentifier(token) || token.identifierHasEscaping || token.start < 0 || token.end > len(source) {
		return false
	}
	if token.kind == tokenQuotedIdentifier {
		return string(source[token.contentStart:token.contentEnd]) == token.text
	}
	return string(source[token.start:token.end]) == token.text
}

func sourceReference(base model.SourceRef, start, end int) model.SourceRef {
	base.Range = model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
	return base
}

func keyword(token sqlToken, word string) bool {
	return token.kind == tokenWord && strings.EqualFold(token.text, word)
}

func symbol(token sqlToken, value string) bool {
	return token.kind == tokenSymbol && token.text == value
}

func isIdentifier(token sqlToken) bool {
	return token.kind == tokenWord || token.kind == tokenQuotedIdentifier
}

func isIdentifierStart(value rune) bool {
	return value == '_' || value == '$' || unicode.IsLetter(value)
}

func isIdentifierPart(value rune) bool {
	return value == '_' || value == '$' || unicode.IsLetter(value) || unicode.IsDigit(value)
}

func isSQLIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

func isSQLWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == '\f'
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
