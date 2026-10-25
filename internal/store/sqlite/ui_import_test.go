package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestOpenIndexUIUpgradesV2AndCoreRejectsV4WithoutMutation(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "81000000-0000-4000-8000-000000000001"))
	options.Create = true

	core, err := openIndex(ctx, options)
	if err != nil {
		t.Fatalf("open default core index: %v", err)
	}
	assertIndexVersions(t, core, 2, 2)
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	options.Create = false
	ui, err := OpenIndexUI(ctx, options)
	if err != nil {
		t.Fatalf("upgrade core index through UI opener: %v", err)
	}
	assertIndexVersions(t, ui, 4, 4)
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(options.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(ctx, options); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("core opener error=%v; want ErrSchemaTooNew", err)
	}
	after, err := os.ReadFile(options.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("core opener changed the schema-4 index while rejecting it")
	}
}

func TestOpenIndexUICreatesSchema4FromFreshIndex(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "81000000-0000-4000-8000-000000000002"))
	options.Create = true
	store, err := OpenIndexUI(ctx, options)
	if err != nil {
		t.Fatalf("create UI-enabled index: %v", err)
	}
	assertIndexVersions(t, store, 4, 4)
	for _, name := range []string{"ui_build_maps", "ui_source_anchors", "ui_build_bindings", "ui_anchor_links"} {
		var count int
		if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("UI migration table %s count=%d; want 1", name, count)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeUIManifestRejectsDuplicateUnknownAndNoncanonicalJSON(t *testing.T) {
	worktree := testUUID(t, "82000000-0000-4000-8000-000000000001")
	manifest, raw := makeT085UIManifest(t, worktree, model.Hash(strings.Repeat("f", 64)), []byte("export const App = () => <button />;\n"))
	if _, _, err := decodeUIManifest(raw); err != nil {
		t.Fatalf("decode valid canonical UI manifest: %v", err)
	}

	duplicate := bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
	unknownValue := uiManifestValue(manifest)
	unknownValue["unexpected"] = true
	unknown, err := model.CanonicalJSON1(unknownValue)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"duplicate member": duplicate,
		"unknown member":   unknown,
		"trailing bytes":   append(append([]byte(nil), raw...), ' '),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeUIManifest(candidate); err == nil {
				t.Fatal("invalid UI manifest was accepted")
			}
		})
	}
}

func TestImportUIManifestValidatesAndCommitsMapBindingAndLinksAtomically(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "83000000-0000-4000-8000-000000000001"))
	options.Create = true
	store, err := OpenIndexUI(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close UI index: %v", err)
		}
	})

	source := []byte("export function App() { return <button>ok</button>; }\n")
	spec := createPublishedUISource(t, store, source)
	manifest, raw := makeT085UIManifest(t, store.WorktreeID(), spec.Key.ProfileID, source)
	request := makeT085UIBindingRequest(t, store, spec, manifest)

	badEvidence := request
	badEvidence.Links = append([]UIAnchorLink(nil), request.Links...)
	badEvidence.Links[0].Evidence = append([]model.SourceRef(nil), request.Links[0].Evidence...)
	badEvidence.Links[0].Evidence[0].Range.EndByte--
	if _, err := store.ImportUIManifest(ctx, raw, badEvidence); err == nil {
		t.Fatal("binding with evidence that does not equal the source anchor was accepted")
	}
	assertUITableCounts(t, store, 0, 0, 0, 0)

	if _, err := store.writer.ExecContext(ctx, `CREATE TRIGGER test_fail_ui_link BEFORE INSERT ON ui_anchor_links
		BEGIN SELECT RAISE(ABORT,'injected UI link write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportUIManifest(ctx, raw, request); err == nil {
		t.Fatal("injected link insertion failure was not returned")
	}
	assertUITableCounts(t, store, 0, 0, 0, 0)
	if _, err := store.writer.ExecContext(ctx, `DROP TRIGGER test_fail_ui_link`); err != nil {
		t.Fatal(err)
	}

	first, err := store.ImportUIManifest(ctx, raw, request)
	if err != nil {
		t.Fatalf("import validated UI map and binding: %v", err)
	}
	if first.AlreadyImported || first.BuildMapID != manifest.BuildMapID {
		t.Fatalf("first import result=%+v; expected newly imported map %s", first, manifest.BuildMapID)
	}
	assertUITableCounts(t, store, 1, len(manifest.Anchors), 1, len(request.Links))

	retry, err := store.ImportUIManifest(ctx, raw, request)
	if err != nil {
		t.Fatalf("retry exact UI map/binding import: %v", err)
	}
	if !retry.AlreadyImported || retry.BindingID != first.BindingID {
		t.Fatalf("retry result=%+v; want idempotent binding %s", retry, first.BindingID)
	}

	secondRequest := request
	secondRequest.ResolverDigest = model.Hash(strings.Repeat("7", 64))
	second, err := store.ImportUIManifest(ctx, raw, secondRequest)
	if err != nil {
		t.Fatalf("import same map with distinct resolver binding: %v", err)
	}
	if second.BindingID == first.BindingID || second.AlreadyImported {
		t.Fatalf("distinct resolver binding result=%+v; want a new immutable binding", second)
	}
	assertUITableCounts(t, store, 1, len(manifest.Anchors), 2, len(request.Links)*2)
}

func TestImportUIManifestRequiresExplicitUIStore(t *testing.T) {
	store := openTestIndex(t, 1_000)
	_, err := store.ImportUIManifest(context.Background(), []byte(`{}`), UIBindingRequest{})
	if !errors.Is(err, ErrUIStoreRequired) {
		t.Fatalf("core store UI import error=%v; want ErrUIStoreRequired", err)
	}
}

func assertIndexVersions(t *testing.T, store *Store, schema, minReader int64) {
	t.Helper()
	var gotSchema, gotReader int64
	if err := store.ReadDB().QueryRowContext(context.Background(), `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&gotSchema, &gotReader); err != nil {
		t.Fatal(err)
	}
	if gotSchema != schema || gotReader != minReader {
		t.Fatalf("schema metadata=%d/%d; want %d/%d", gotSchema, gotReader, schema, minReader)
	}
}

func createPublishedUISource(t *testing.T, store *Store, source []byte) GenerationSpec {
	t.Helper()
	ctx := context.Background()
	spec := testGenerationSpec(t, "84000000-0000-4000-8000-000000000001", "85000000-0000-4000-8000-000000000001", "8")
	if _, err := store.CreateGeneration(ctx, spec); err != nil {
		t.Fatalf("create UI source generation: %v", err)
	}
	hash := model.SHA256(source)
	if err := store.WriteGenerationBatch(ctx, spec.Key, manifestWriteOverhead, func(batch *GenerationBatch) error {
		if _, err := batch.Exec(`INSERT INTO content_blobs(content_hash,bytes,byte_length) VALUES(?,?,?)`, string(hash), source, len(source)); err != nil {
			return err
		}
		_, err := batch.Exec(`INSERT INTO files(generation_id,path,content_hash,kind,status,byte_length,package_key,generated,reason_code)
			VALUES(?,? ,?,'source','indexed',?,NULL,0,NULL)`, string(spec.Key.GenerationID), "src/App.tsx", string(hash), len(source))
		return err
	}); err != nil {
		t.Fatalf("stage UI source bytes: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
		t.Fatalf("publish UI source generation: %v", err)
	}
	return spec
}

func makeT085UIManifest(t *testing.T, worktree model.UUID, profile model.Hash, source []byte) (UIBuildManifest, []byte) {
	t.Helper()
	sourceHash := model.SHA256(source)
	sources := []any{map[string]any{"path": "src/App.tsx", "content_hash": sourceHash}}
	inputDigest, err := model.HashValue("ui-input-manifest-v1", sources)
	if err != nil {
		t.Fatal(err)
	}
	providerDigest := model.Hash(strings.Repeat("1", 64))
	manifest := UIBuildManifest{
		SchemaVersion: 1, WorktreeID: worktree, ProfileID: profile,
		ProviderVersion: "vite-inspector/1.0.0", ProviderDigest: providerDigest,
		BuildConfigDigest: model.Hash(strings.Repeat("2", 64)), InputManifestDigest: inputDigest,
		Mode: "development", ModuleCoverage: "loaded_modules_only",
		Sources: []UIManifestSource{{Path: "src/App.tsx", ContentHash: sourceHash}},
	}
	rangeValue := model.ByteRange{StartByte: 0, EndByte: int64(len(source))}
	anchorID, err := model.HashValue("anchor-v1", map[string]any{
		"path": "src/App.tsx", "content_hash": sourceHash, "kind": "element_site",
		"start_byte": rangeValue.StartByte, "end_byte": rangeValue.EndByte, "provider_digest": providerDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest.Anchors = []UIManifestAnchor{{
		AnchorID: anchorID, Kind: "element_site", Path: "src/App.tsx", ContentHash: sourceHash,
		Range: rangeValue,
	}}
	manifest.BuildMapID, err = model.HashValue("build-map-v1", uiBuildMapValue(manifest))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := model.CanonicalJSON1(uiManifestValue(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func makeT085UIBindingRequest(t *testing.T, store *Store, spec GenerationSpec, manifest UIBuildManifest) UIBindingRequest {
	t.Helper()
	anchor := manifest.Anchors[0]
	contentHash := anchor.ContentHash
	target, err := model.CanonicalJSON1(map[string]any{
		"kind": "file", "worktree_id": store.WorktreeID(), "profile_id": spec.Key.ProfileID,
		"generation_id": spec.Key.GenerationID, "path": anchor.Path, "content_hash": contentHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := model.SourceRef{
		WorktreeID: store.WorktreeID(), ProfileID: spec.Key.ProfileID, GenerationID: spec.Key.GenerationID,
		Path: anchor.Path, ContentHash: anchor.ContentHash, Range: anchor.Range,
	}
	return UIBindingRequest{
		SourceGenerationID: spec.Key.GenerationID, ProfileID: spec.Key.ProfileID,
		PolicyDigest: spec.PolicyDigest, ResolverDigest: model.Hash(strings.Repeat("6", 64)),
		Links: []UIAnchorLink{{
			LinkID: "candidate-1", AnchorID: anchor.AnchorID, TargetRef: json.RawMessage(target),
			ResolutionStatus: "candidate", Evidence: []model.SourceRef{evidence},
		}},
	}
}

func assertUITableCounts(t *testing.T, store *Store, maps, anchors, bindings, links int) {
	t.Helper()
	var gotMaps, gotAnchors, gotBindings, gotLinks int
	err := store.ReadDB().QueryRowContext(context.Background(), `SELECT
		(SELECT count(*) FROM ui_build_maps),
		(SELECT count(*) FROM ui_source_anchors),
		(SELECT count(*) FROM ui_build_bindings),
		(SELECT count(*) FROM ui_anchor_links)`).Scan(&gotMaps, &gotAnchors, &gotBindings, &gotLinks)
	if err != nil {
		t.Fatal(err)
	}
	if gotMaps != maps || gotAnchors != anchors || gotBindings != bindings || gotLinks != links {
		t.Fatalf("UI metadata counts=%d/%d/%d/%d; want %d/%d/%d/%d", gotMaps, gotAnchors, gotBindings, gotLinks, maps, anchors, bindings, links)
	}
}
