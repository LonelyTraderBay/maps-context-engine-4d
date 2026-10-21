package sqlcontract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	sqlTestWorktreeID   = model.UUID("11111111-1111-4111-8111-111111111111")
	sqlTestGenerationID = model.UUID("22222222-2222-4222-8222-222222222222")
	sqlTestProfileID    = model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
)

func oracleSources() []Source {
	schema := []byte("CREATE TABLE app.orders (id BIGINT) ENGINE=InnoDB;\n" +
		"CREATE VIEW app.order_view AS SELECT * FROM app.orders;\n" +
		"CREATE TRIGGER app.orders_bi BEFORE INSERT ON app.orders FOR EACH ROW SET NEW.id = NEW.id;\n")
	migration := []byte("-- Captured migration source; applied state is not observable here.\n")
	return []Source{
		{Ref: sourceRef("schema.sql", schema, 0, int64(len(schema))), Role: RoleSchemaSource, Bytes: schema},
		{Ref: sourceRef("migrations/001_note.sql", migration, 0, int64(len(migration))), Role: RoleMigrationSource, Bytes: migration},
	}
}

func oracleResult(sources []Source) Result {
	schema := sources[0].Bytes
	tableEnd := strings.IndexByte(string(schema), ';')
	columnStart := strings.Index(string(schema), "id BIGINT")
	viewStart := strings.Index(string(schema), "CREATE VIEW")
	viewEnd := viewStart + strings.IndexByte(string(schema[viewStart:]), ';')
	triggerStart := strings.Index(string(schema), "CREATE TRIGGER")
	triggerEnd := triggerStart + strings.IndexByte(string(schema[triggerStart:]), ';')
	fromStart := strings.Index(string(schema[viewStart:viewEnd]), "FROM app.orders") + viewStart + len("FROM ")
	onStart := strings.Index(string(schema[triggerStart:triggerEnd]), "ON app.orders") + triggerStart + len("ON ")

	tableRef := sourceRef("schema.sql", schema, 0, int64(tableEnd))
	columnRef := sourceRef("schema.sql", schema, int64(columnStart), int64(columnStart+len("id BIGINT")))
	columnEvidence := sourceRef("schema.sql", schema, int64(columnStart), int64(columnStart+len("id")))
	viewRef := sourceRef("schema.sql", schema, int64(viewStart), int64(viewEnd))
	triggerRef := sourceRef("schema.sql", schema, int64(triggerStart), int64(triggerEnd))
	viewEvidence := sourceRef("schema.sql", schema, int64(fromStart), int64(fromStart+len("app.orders")))
	triggerEvidence := sourceRef("schema.sql", schema, int64(onStart), int64(onStart+len("app.orders")))
	tableID := ObjectIDFor(ObjectTable, tableRef)
	columnID := ObjectIDFor(ObjectColumn, columnRef)
	viewID := ObjectIDFor(ObjectView, viewRef)
	triggerID := ObjectIDFor(ObjectTrigger, triggerRef)

	return Result{
		FormatVersion: ResultFormatVersion,
		Profile:       Profile{Dialect: DialectMySQL, ParserVersion: 1},
		Coverage:      CoverageComplete,
		Objects: []Object{
			{ID: tableID, Kind: ObjectTable, Schema: "app", Name: "orders", Engine: &EngineIdentity{Class: EngineInnoDB, DeclaredName: "InnoDB"}, SourceRef: tableRef},
			{ID: columnID, Kind: ObjectColumn, Schema: "app", Name: "id", ParentID: tableID, SourceRef: columnRef},
			{ID: viewID, Kind: ObjectView, Schema: "app", Name: "order_view", SourceRef: viewRef},
			{ID: triggerID, Kind: ObjectTrigger, Schema: "app", Name: "orders_bi", SourceRef: triggerRef},
		},
		Relations: []Relation{
			{FromID: tableID, ToID: columnID, Kind: RelationTableHasColumn, SourceRef: columnEvidence},
			{FromID: viewID, ToID: tableID, Kind: RelationViewReadsTable, SourceRef: viewEvidence},
			{FromID: triggerID, ToID: tableID, Kind: RelationTriggerOnTable, SourceRef: triggerEvidence},
		},
		Diagnostics: []Diagnostic{},
		Migrations:  []Migration{{Path: sources[1].Ref.Path, Order: 1, AppliedState: MigrationStateUnknown, SourceRef: sources[1].Ref}},
	}
}

func sourceRef(path string, raw []byte, start, end int64) model.SourceRef {
	return model.SourceRef{
		WorktreeID:   sqlTestWorktreeID,
		ProfileID:    sqlTestProfileID,
		GenerationID: sqlTestGenerationID,
		Path:         path,
		ContentHash:  model.SHA256(raw),
		Range:        model.ByteRange{StartByte: start, EndByte: end},
	}
}

func TestMySQLLineageOracleIsClosedAndSourceBound(t *testing.T) {
	sources := oracleSources()
	want := oracleResult(sources)
	if err := want.ValidateAgainst(sources); err != nil {
		t.Fatalf("manual SQL lineage oracle rejected: %v", err)
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal oracle: %v", err)
	}
	got, err := DecodeResult(encoded)
	if err != nil {
		t.Fatalf("decode oracle: %v", err)
	}
	if err := got.ValidateAgainst(sources); err != nil {
		t.Fatalf("decoded SQL lineage oracle rejected: %v", err)
	}
	if !equalJSON(got, want) {
		t.Fatalf("JSON oracle changed typed SQL facts\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSQLContractRejectsUnknownKindsAndEngineConflation(t *testing.T) {
	sources := oracleSources()
	result := oracleResult(sources)
	result.Objects[0].Engine = &EngineIdentity{Class: EngineUnknown, DeclaredName: "InnoDB"}
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown engine with declared name error = %v, want invalid SQL contract", err)
	}

	result = oracleResult(sources)
	result.Objects[0].Engine = &EngineIdentity{Class: EngineUnsupported, DeclaredName: "InnoDB"}
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("InnoDB classified as unsupported error = %v, want invalid SQL contract", err)
	}

	result = oracleResult(sources)
	result.Objects[2].Kind = "stored_program"
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown object kind error = %v, want invalid SQL contract", err)
	}
}

func TestSQLContractRejectsNameOnlyAndOutOfRangeLineage(t *testing.T) {
	sources := oracleSources()
	result := oracleResult(sources)
	result.Objects[2].Schema = ""
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unqualified view lineage error = %v, want invalid SQL contract", err)
	}

	result = oracleResult(sources)
	result.Relations[1].SourceRef.Range.StartByte = result.Objects[2].SourceRef.Range.EndByte
	result.Relations[1].SourceRef.Range.EndByte = result.Objects[2].SourceRef.Range.EndByte + 1
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("lineage outside declaration error = %v, want invalid SQL contract", err)
	}

	result = oracleResult(sources)
	result.Relations[1].SourceRef.Range.StartByte++
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("relation token not matching target error = %v, want invalid SQL contract", err)
	}
}

func TestSQLContractRejectsSameNameTableLineageAcrossSources(t *testing.T) {
	sources := oracleSources()
	duplicateBytes := []byte("CREATE TABLE app.orders (id INT);\n")
	duplicateSource := Source{
		Ref:   sourceRef("zzz_duplicate.sql", duplicateBytes, 0, int64(len(duplicateBytes))),
		Role:  RoleSchemaSource,
		Bytes: duplicateBytes,
	}
	sources = append(sources, duplicateSource)
	result := oracleResult(sources[:2])
	tableSpan := sourceRef(duplicateSource.Ref.Path, duplicateBytes, 0, int64(strings.IndexByte(string(duplicateBytes), ';')))
	result.Objects = append(result.Objects, Object{
		ID: ObjectIDFor(ObjectTable, tableSpan), Kind: ObjectTable, Schema: "app", Name: "orders",
		Engine: &EngineIdentity{Class: EngineUnknown}, SourceRef: tableSpan,
	})
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("same-name table lineage error = %v, want invalid SQL contract", err)
	}
}

func TestSQLContractRejectsSourceHashAndAppliedMigrationClaims(t *testing.T) {
	sources := oracleSources()
	result := oracleResult(sources)
	result.Objects[0].SourceRef.ContentHash = model.Hash(strings.Repeat("b", 64))
	result.Objects[0].ID = ObjectIDFor(ObjectTable, result.Objects[0].SourceRef)
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("mismatched source hash error = %v, want invalid SQL contract", err)
	}

	result = oracleResult(sources)
	result.Migrations[0].AppliedState = "applied"
	if err := result.ValidateAgainst(sources); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("migration applied-state claim error = %v, want invalid SQL contract", err)
	}
}

func TestDecodeResultRejectsUnknownDuplicateAndTrailingJSON(t *testing.T) {
	encoded, err := json.Marshal(oracleResult(oracleSources()))
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(encoded), `"format_version":1`, `"format_version":1,"extra":true`, 1)
	if _, err := DecodeResult([]byte(unknown)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown field error = %v, want invalid SQL contract", err)
	}
	duplicate := strings.Replace(string(encoded), `"format_version":1`, `"format_version":1,"format_version":1`, 1)
	if _, err := DecodeResult([]byte(duplicate)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("duplicate JSON member error = %v, want invalid SQL contract", err)
	}
	if _, err := DecodeResult(append(encoded, []byte(` {}`)...)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("trailing JSON error = %v, want invalid SQL contract", err)
	}
}

func TestSQLObjectIdentityDoesNotUseNameAlone(t *testing.T) {
	first := sourceRef("schema-a.sql", []byte("same"), 0, 4)
	second := sourceRef("schema-b.sql", []byte("same"), 0, 4)
	if ObjectIDFor(ObjectTable, first) == ObjectIDFor(ObjectTable, second) {
		t.Fatal("same-name objects from different captured sources shared identity")
	}
}

func equalJSON(left, right Result) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
