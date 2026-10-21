package sqlcontract

import (
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestAnalyzeMatchesIndependentMySQLOracle(t *testing.T) {
	sources := oracleSources()
	got, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want := oracleResult(sources)
	if !equalJSON(got, want) {
		t.Fatalf("analyzer differs from independent oracle\n got: %#v\nwant: %#v", got, want)
	}
}

func TestAnalyzeLinksProjectedViewColumnsOnlyWithinTheCapturedSnapshot(t *testing.T) {
	tableBytes := []byte("CREATE TABLE app.orders (id BIGINT, customer_id BIGINT) ENGINE=InnoDB;")
	viewBytes := []byte("CREATE VIEW app.order_ids AS SELECT id, customer_id AS customer FROM app.orders;")
	sources := []Source{
		{Ref: sourceRef("schema.sql", tableBytes, 0, int64(len(tableBytes))), Role: RoleSchemaSource, Bytes: tableBytes},
		{Ref: sourceRef("views.sql", viewBytes, 0, int64(len(viewBytes))), Role: RoleSchemaSource, Bytes: viewBytes},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.Coverage != CoverageComplete {
		t.Fatalf("coverage = %q, want complete: %#v", result.Coverage, result.Diagnostics)
	}
	var tableReads, columnReads int
	for _, relation := range result.Relations {
		switch relation.Kind {
		case RelationViewReadsTable:
			tableReads++
		case RelationViewReadsColumn:
			columnReads++
		}
	}
	if tableReads != 1 || columnReads != 2 {
		t.Fatalf("view lineage table=%d columns=%d, want 1 and 2", tableReads, columnReads)
	}
	if err := result.ValidateAgainst(sources); err != nil {
		t.Fatalf("column lineage result failed its contract: %v", err)
	}
}

func TestAnalyzeDoesNotJoinSameNamesAcrossSourceVersions(t *testing.T) {
	tableBytes := []byte("CREATE TABLE app.orders (id BIGINT) ENGINE=InnoDB;")
	viewBytes := []byte("CREATE VIEW app.order_ids AS SELECT id FROM app.orders;")
	tableSource := Source{Ref: sourceRef("schema.sql", tableBytes, 0, int64(len(tableBytes))), Role: RoleSchemaSource, Bytes: tableBytes}
	viewRef := sourceRef("views.sql", viewBytes, 0, int64(len(viewBytes)))
	viewRef.GenerationID = model.UUID("33333333-3333-4333-8333-333333333333")
	viewSource := Source{Ref: viewRef, Role: RoleSchemaSource, Bytes: viewBytes}
	result, err := Analyze([]Source{viewSource, tableSource}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, relation := range result.Relations {
		if relation.Kind == RelationViewReadsTable || relation.Kind == RelationViewReadsColumn {
			t.Fatalf("cross-snapshot relation was fabricated: %#v", relation)
		}
	}
	if !hasDiagnostic(result, DiagnosticUnresolvedReference) {
		t.Fatalf("cross-snapshot gap missing: %#v", result.Diagnostics)
	}
}

func TestAnalyzeResolvesDuplicateNamesOnlyInsideEachCapturedSnapshot(t *testing.T) {
	statement := []byte("CREATE TABLE app.orders (id BIGINT) ENGINE=InnoDB;")
	view := []byte("CREATE VIEW app.order_ids AS SELECT id FROM app.orders;")
	generationOne := model.UUID("44444444-4444-4444-8444-444444444444")
	generationTwo := model.UUID("55555555-5555-4555-8555-555555555555")
	tableOneRef := sourceRef("v1-schema.sql", statement, 0, int64(len(statement)))
	viewOneRef := sourceRef("v1-view.sql", view, 0, int64(len(view)))
	tableOneRef.GenerationID, viewOneRef.GenerationID = generationOne, generationOne
	tableTwoRef := sourceRef("v2-schema.sql", statement, 0, int64(len(statement)))
	viewTwoRef := sourceRef("v2-view.sql", view, 0, int64(len(view)))
	tableTwoRef.GenerationID, viewTwoRef.GenerationID = generationTwo, generationTwo
	sources := []Source{
		{Ref: tableOneRef, Role: RoleSchemaSource, Bytes: statement},
		{Ref: viewOneRef, Role: RoleSchemaSource, Bytes: view},
		{Ref: tableTwoRef, Role: RoleSchemaSource, Bytes: statement},
		{Ref: viewTwoRef, Role: RoleSchemaSource, Bytes: view},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze versioned captures: %v", err)
	}
	var tableReads, columnReads int
	for _, relation := range result.Relations {
		switch relation.Kind {
		case RelationViewReadsTable:
			tableReads++
		case RelationViewReadsColumn:
			columnReads++
		}
	}
	if result.Coverage != CoverageComplete || tableReads != 2 || columnReads != 2 {
		t.Fatalf("version-local lineage = coverage %q, table=%d column=%d: %#v", result.Coverage, tableReads, columnReads, result)
	}
	if err := result.ValidateAgainst(sources); err != nil {
		t.Fatalf("version-local lineage violated the result contract: %v", err)
	}
}

func TestAnalyzeWithholdsColumnLineageForJoinedViews(t *testing.T) {
	first := []byte("CREATE TABLE app.first_table (id INT) ENGINE=InnoDB;")
	second := []byte("CREATE TABLE app.second_table (id INT) ENGINE=InnoDB;")
	view := []byte("CREATE VIEW app.joined AS SELECT id FROM app.first_table JOIN app.second_table ON first_table.id = second_table.id;")
	sources := []Source{
		{Ref: sourceRef("first.sql", first, 0, int64(len(first))), Role: RoleSchemaSource, Bytes: first},
		{Ref: sourceRef("second.sql", second, 0, int64(len(second))), Role: RoleSchemaSource, Bytes: second},
		{Ref: sourceRef("view.sql", view, 0, int64(len(view))), Role: RoleSchemaSource, Bytes: view},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze joined view: %v", err)
	}
	if !hasDiagnostic(result, DiagnosticUnsupportedSyntax) {
		t.Fatalf("joined view did not retain its unsupported-source gap: %#v", result.Diagnostics)
	}
	for _, relation := range result.Relations {
		if relation.Kind == RelationViewReadsColumn {
			t.Fatalf("ambiguous joined projection fabricated column lineage: %#v", relation)
		}
	}
}

func TestAnalyzeDoesNotInferDdlFactsFromDynamicSQLStatements(t *testing.T) {
	raw := []byte("CREATE TABLE app.orders (id INT) ENGINE=InnoDB;\nCREATE VIEW app.dynamic_view AS SELECT id FROM app.orders WHERE id = PREPARE stmt FROM @sql;")
	source := Source{Ref: sourceRef("dynamic-view.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze dynamic SQL: %v", err)
	}
	if !hasDiagnostic(result, DiagnosticDynamicSQL) {
		t.Fatalf("dynamic SQL gap missing: %#v", result.Diagnostics)
	}
	for _, object := range result.Objects {
		if object.Kind == ObjectView && object.Name == "dynamic_view" {
			t.Fatalf("dynamic view statement was promoted to a source fact: %#v", object)
		}
	}
	for _, relation := range result.Relations {
		if relation.Kind == RelationViewReadsTable || relation.Kind == RelationViewReadsColumn {
			t.Fatalf("dynamic view statement fabricated lineage: %#v", relation)
		}
	}
}

func TestAnalyzeRejectsStatementsBeyondTheTokenBound(t *testing.T) {
	raw := []byte("SELECT " + strings.Repeat("a ", maxSQLTokensPerStatement))
	source := Source{Ref: sourceRef("oversized.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	_, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("token-bound violation error = %v, want ErrInvalidContract", err)
	}
}

func TestAnalyzeRejectsCapturesBeyondTheAggregateTokenBound(t *testing.T) {
	const tokensPerSource = 90_000
	sources := make([]Source, 0, 6)
	for index := 0; index < 6; index++ {
		raw := []byte("SELECT " + strings.Repeat("a ", tokensPerSource))
		path := "source-" + string(rune('a'+index)) + ".sql"
		sources = append(sources, Source{
			Ref: sourceRef(path, raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw,
		})
	}
	_, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("aggregate token-bound violation error = %v, want ErrInvalidContract", err)
	}
}

func TestAnalyzeRejectsSourcesBeyondTheScanIssueBound(t *testing.T) {
	raw := []byte(strings.Repeat("/*!1*/", maxSQLScanIssuesPerSource+1))
	source := Source{Ref: sourceRef("too-many-gaps.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	_, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("scan-issue bound violation error = %v, want ErrInvalidContract", err)
	}
}

func TestAnalyzeKeepsEngineIdentityAndMigrationStateExplicit(t *testing.T) {
	innodb := []byte("CREATE TABLE app.innodb_table (id INT) ENGINE=InnoDB;")
	ndb := []byte("CREATE TABLE app.ndb_table (id INT) ENGINE=NDBCLUSTER;")
	aria := []byte("CREATE TABLE app.aria_table (id INT) ENGINE=Aria;")
	unknown := []byte("CREATE TABLE app.unknown_table (id INT);")
	migration010 := []byte("-- migration 10\n")
	migration002 := []byte("-- migration 2\n")
	sources := []Source{
		{Ref: sourceRef("schema/innodb.sql", innodb, 0, int64(len(innodb))), Role: RoleSchemaSource, Bytes: innodb},
		{Ref: sourceRef("schema/ndb.sql", ndb, 0, int64(len(ndb))), Role: RoleSchemaSource, Bytes: ndb},
		{Ref: sourceRef("schema/aria.sql", aria, 0, int64(len(aria))), Role: RoleSchemaSource, Bytes: aria},
		{Ref: sourceRef("schema/unknown.sql", unknown, 0, int64(len(unknown))), Role: RoleSchemaSource, Bytes: unknown},
		{Ref: sourceRef("migrations/010.sql", migration010, 0, int64(len(migration010))), Role: RoleMigrationSource, Bytes: migration010},
		{Ref: sourceRef("migrations/002.sql", migration002, 0, int64(len(migration002))), Role: RoleMigrationSource, Bytes: migration002},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	engines := make(map[string]EngineIdentity)
	for _, object := range result.Objects {
		if object.Kind == ObjectTable {
			engines[object.Name] = *object.Engine
		}
	}
	if engines["innodb_table"].Class != EngineInnoDB || engines["ndb_table"].Class != EngineNDBCluster ||
		engines["aria_table"].Class != EngineUnsupported || engines["aria_table"].DeclaredName != "Aria" ||
		engines["unknown_table"].Class != EngineUnknown {
		t.Fatalf("engine identities were conflated: %#v", engines)
	}
	if len(result.Migrations) != 2 || result.Migrations[0].Path != "migrations/002.sql" || result.Migrations[1].Path != "migrations/010.sql" {
		t.Fatalf("migration ordering = %#v", result.Migrations)
	}
	for _, migration := range result.Migrations {
		if migration.AppliedState != MigrationStateUnknown {
			t.Fatalf("source-only migration claimed applied state: %#v", migration)
		}
	}
	if !hasDiagnostic(result, DiagnosticUnsupportedEngine) || !hasDiagnostic(result, DiagnosticUnknownEngine) {
		t.Fatalf("engine gaps are missing: %#v", result.Diagnostics)
	}
}

func TestAnalyzeHandlesClientDelimitersAndQuarantinesDynamicSQL(t *testing.T) {
	raw := []byte("DELIMITER $$\nCREATE PROCEDURE app.build_query() BEGIN SET @sql = CONCAT('SELECT ', 'id'); PREPARE stmt FROM @sql; EXECUTE stmt; END$$\nDELIMITER ;\n")
	source := Source{Ref: sourceRef("routines.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(result.Objects) != 1 || result.Objects[0].Kind != ObjectProcedure || result.Objects[0].Name != "build_query" {
		t.Fatalf("routine header was not retained: %#v", result.Objects)
	}
	if hasDiagnostic(result, DiagnosticMalformedSource) || !hasDiagnostic(result, DiagnosticDynamicSQL) ||
		!hasDiagnostic(result, DiagnosticUnsupportedSyntax) || len(result.Relations) != 0 {
		t.Fatalf("routine body gaps or fabricated lineage: %#v", result)
	}
}

func TestAnalyzeRequiresACompleteTriggerHeaderBeforeLinking(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("CREATE TRIGGER app.no_row BEFORE INSERT ON app.orders;"),
		[]byte("CREATE TRIGGER app.no_body BEFORE INSERT ON app.orders FOR EACH ROW;"),
		[]byte("CREATE TRIGGER app.noisy BEFORE INSERT ignored ON app.orders FOR EACH ROW SET @x = 1;"),
	} {
		source := Source{Ref: sourceRef("trigger.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
		result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
		if err != nil {
			t.Fatalf("Analyze malformed trigger header %q: %v", raw, err)
		}
		if len(result.Objects) != 1 || result.Objects[0].Kind != ObjectTrigger || !hasDiagnostic(result, DiagnosticUnsupportedSyntax) {
			t.Fatalf("incomplete trigger was not retained as a gap: %#v", result)
		}
		for _, relation := range result.Relations {
			if relation.Kind == RelationTriggerOnTable {
				t.Fatalf("incomplete trigger fabricated table lineage: %#v", relation)
			}
		}
	}
}

func TestAnalyzeStopsAtSQLModeDependentBackslashStrings(t *testing.T) {
	raw := []byte("CREATE TABLE app.before_gap (id INT) ENGINE=InnoDB;\nCREATE VIEW app.ambiguous AS SELECT 'value\\';\nCREATE TABLE app.must_not_be_inferred (id INT) ENGINE=InnoDB;")
	source := Source{Ref: sourceRef("mode-dependent.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze mode-dependent string: %v", err)
	}
	var tables []string
	for _, object := range result.Objects {
		if object.Kind == ObjectTable {
			tables = append(tables, object.Name)
		}
	}
	if len(tables) != 1 || tables[0] != "before_gap" || !hasDiagnostic(result, DiagnosticUnsupportedSyntax) {
		t.Fatalf("mode-dependent string was split into fabricated SQL: tables=%v result=%#v", tables, result)
	}
}

func TestAnalyzeProtectsCommentsAndQuotedSemicolonsAndRejectsExecutableComments(t *testing.T) {
	raw := []byte("-- fake CREATE TABLE app.fake (id INT);\n/*!50100 CREATE TABLE app.hidden (id INT) ENGINE=InnoDB */;\nCREATE TABLE app.visible (note VARCHAR(20) DEFAULT 'a;--b') ENGINE=InnoDB;\n")
	source := Source{Ref: sourceRef("schema.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	names := make([]string, 0)
	for _, object := range result.Objects {
		if object.Kind == ObjectTable {
			names = append(names, object.Name)
		}
	}
	if len(names) != 1 || names[0] != "visible" {
		t.Fatalf("comment/string handling fabricated or lost tables: %v", names)
	}
	if !hasDiagnostic(result, DiagnosticExecutableComment) {
		t.Fatalf("conditional executable comment was not reported: %#v", result.Diagnostics)
	}
	var table Object
	for _, object := range result.Objects {
		if object.Kind == ObjectTable {
			table = object
		}
	}
	declaration := string(raw[table.SourceRef.Range.StartByte:table.SourceRef.Range.EndByte])
	if !strings.HasPrefix(declaration, "CREATE TABLE app.visible") || !strings.Contains(declaration, "DEFAULT 'a;--b'") ||
		!strings.HasSuffix(declaration, "ENGINE=InnoDB") {
		t.Fatalf("table declaration byte range is wrong: %q", declaration)
	}
}

func TestAnalyzeReportsMalformedQuotesAndUnsupportedDialectsAsGaps(t *testing.T) {
	raw := []byte("CREATE TABLE app.good (id INT) ENGINE=InnoDB;\nCREATE TABLE `app.bad (id INT) ENGINE=InnoDB;")
	source := Source{Ref: sourceRef("schema.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !hasDiagnostic(result, DiagnosticMalformedSource) {
		t.Fatalf("malformed identifier quote was not reported: %#v", result.Diagnostics)
	}
	tables := 0
	for _, object := range result.Objects {
		if object.Kind == ObjectTable {
			tables++
		}
	}
	if tables != 1 {
		t.Fatalf("malformed suffix produced table facts: %d", tables)
	}
	if _, err := Analyze([]Source{source}, Profile{Dialect: "postgres", ParserVersion: 1}); err == nil {
		t.Fatal("non-MySQL profile was accepted")
	}
}

func TestAnalyzeMarksUnsupportedTableOptionsAndMySQLDialectMarkersAsGaps(t *testing.T) {
	good := []byte("CREATE TABLE app.good_options (id INT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 AUTO_INCREMENT=4;")
	partitioned := []byte("CREATE TABLE app.partitioned (id INT) ENGINE=InnoDB PARTITION BY HASH(id);")
	view := []byte("CREATE VIEW app.cast_view AS SELECT id::text FROM app.good_options;")
	sources := []Source{
		{Ref: sourceRef("schema/good.sql", good, 0, int64(len(good))), Role: RoleSchemaSource, Bytes: good},
		{Ref: sourceRef("schema/partitioned.sql", partitioned, 0, int64(len(partitioned))), Role: RoleSchemaSource, Bytes: partitioned},
		{Ref: sourceRef("views/cast.sql", view, 0, int64(len(view))), Role: RoleSchemaSource, Bytes: view},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !hasDiagnostic(result, DiagnosticUnsupportedSyntax) {
		t.Fatalf("unsupported DDL or dialect syntax did not retain a gap: %#v", result.Diagnostics)
	}
	for _, relation := range result.Relations {
		if relation.Kind == RelationViewReadsColumn {
			t.Fatalf("PostgreSQL cast produced a column fact: %#v", relation)
		}
	}
	if _, err := Analyze(sources, Profile{Dialect: "postgres", ParserVersion: 1}); err == nil {
		t.Fatal("non-MySQL profile was accepted")
	}
}

func TestAnalyzeUsesUTF8ByteRangesAndConservativeQuotedNames(t *testing.T) {
	raw := []byte("CREATE TABLE app.注文 (`名` VARCHAR(20)) ENGINE=InnoDB;\nCREATE VIEW app.注文_view AS SELECT `名` FROM app.注文;")
	source := Source{Ref: sourceRef("unicode.sql", raw, 0, int64(len(raw))), Role: RoleSchemaSource, Bytes: raw}
	result, err := Analyze([]Source{source}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.Coverage != CoverageComplete || len(result.Relations) != 3 {
		t.Fatalf("UTF-8/quoted column lineage = %#v", result)
	}
	for _, relation := range result.Relations {
		if err := relation.SourceRef.ValidateAgainst(raw); err != nil {
			t.Fatalf("relation byte range is invalid: %#v: %v", relation, err)
		}
	}

	escaped := []byte("CREATE TABLE app.escaped (`a``b` INT) ENGINE=InnoDB;")
	escapedSource := Source{Ref: sourceRef("escaped.sql", escaped, 0, int64(len(escaped))), Role: RoleSchemaSource, Bytes: escaped}
	escapedResult, err := Analyze([]Source{escapedSource}, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze escaped identifier: %v", err)
	}
	for _, object := range escapedResult.Objects {
		if object.Kind == ObjectColumn {
			t.Fatalf("escaped identifier emitted a partial column identity: %#v", object)
		}
	}
	if !hasDiagnostic(escapedResult, DiagnosticUnsupportedSyntax) {
		t.Fatalf("escaped identifier gap missing: %#v", escapedResult.Diagnostics)
	}
}

func TestAnalyzeSupportsPlainBacktickQualifiedMySQLNames(t *testing.T) {
	table := []byte("CREATE TABLE `app`.`orders` (`id` INT) ENGINE=InnoDB;")
	view := []byte("CREATE VIEW `app`.`order_ids` AS SELECT `id` FROM `app`.`orders`;")
	sources := []Source{
		{Ref: sourceRef("quoted-table.sql", table, 0, int64(len(table))), Role: RoleSchemaSource, Bytes: table},
		{Ref: sourceRef("quoted-view.sql", view, 0, int64(len(view))), Role: RoleSchemaSource, Bytes: view},
	}
	result, err := Analyze(sources, Profile{Dialect: DialectMySQL, ParserVersion: 1})
	if err != nil {
		t.Fatalf("Analyze backtick-qualified identifiers: %v", err)
	}
	if result.Coverage != CoverageComplete || len(result.Relations) != 3 {
		t.Fatalf("backtick-qualified lineage = %#v", result)
	}
	if err := result.ValidateAgainst(sources); err != nil {
		t.Fatalf("backtick-qualified evidence failed validation: %v", err)
	}
}

func hasDiagnostic(result Result, code DiagnosticCode) bool {
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
