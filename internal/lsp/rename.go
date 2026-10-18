package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	signedcursor "github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const (
	maxRenamePreviewBytes = int64(32 << 20)
	maxRenamePageSize     = 512
)

var ErrInvalidRenameName = errors.New("new name is not a valid Go identifier")

type RenameProvider struct {
	results *ResultStore
	cursors *signedcursor.CursorCodec
}

type RenameRequest struct {
	Identity        ResultIdentity
	Snapshot        WorkspaceSnapshot
	Profile         GoProfile
	Policy          ExecutionPolicy
	Source          model.SourceRef
	NewName         string
	PageSize        int
	StartupTimeout  time.Duration
	ReadTimeout     time.Duration
	RenameTimeout   time.Duration
	ShutdownTimeout time.Duration
}

type RenamePreview struct {
	ResultRef           ResultRef         `json:"result_ref"`
	Edits               []json.RawMessage `json:"edits"`
	NextCursor          string            `json:"next_cursor,omitempty"`
	Position            int64             `json:"position"`
	HasMore             bool              `json:"has_more"`
	Prepared            PreparedRename    `json:"prepared"`
	Warnings            []string          `json:"warnings,omitempty"`
	ValidationRequired  bool              `json:"validation_required"`
	Applied             bool              `json:"applied"`
	SourceMutationCount int64             `json:"source_mutation_count"`
}

type RenameEdit struct {
	Path        string          `json:"path"`
	ContentHash model.Hash      `json:"content_hash"`
	Version     int64           `json:"version"`
	Range       model.ByteRange `json:"range"`
	NewText     string          `json:"new_text"`
}

type workspaceEdit struct {
	Changes           map[string][]lspTextEdit   `json:"changes,omitempty"`
	DocumentChanges   []json.RawMessage          `json:"documentChanges,omitempty"`
	ChangeAnnotations map[string]json.RawMessage `json:"changeAnnotations,omitempty"`
}

type lspTextDocumentEdit struct {
	TextDocument struct {
		URI     string          `json:"uri"`
		Version json.RawMessage `json:"version"`
	} `json:"textDocument"`
	Edits []lspTextEdit `json:"edits"`
}

type lspTextEdit struct {
	Range        Range  `json:"range"`
	NewText      string `json:"newText"`
	AnnotationID string `json:"annotationId,omitempty"`
}

type validatedRenameEdit struct {
	edit RenameEdit
	wire json.RawMessage
}

type renameCandidate struct {
	uri  string
	text lspTextEdit
}

func NewRenameProvider(results *ResultStore, cursors *signedcursor.CursorCodec) (*RenameProvider, error) {
	if results == nil || cursors == nil {
		return nil, ErrResultInvalid
	}
	return &RenameProvider{results: results, cursors: cursors}, nil
}

func (provider *RenameProvider) Preview(ctx context.Context, request RenameRequest) (RenamePreview, error) {
	if provider == nil || provider.results == nil || provider.cursors == nil || ctx == nil || !tokenIsIdentifier(request.NewName) ||
		request.PageSize < 0 || request.PageSize > maxRenamePageSize || validateResultIdentity(request.Identity) != nil ||
		!sameScope(request.Identity.Scope, request.Snapshot.Scope) || !strings.EqualFold(filepath.Ext(request.Source.Path), ".go") ||
		!sourcePathInModuleRoots(request.Source.Path, request.Profile.ModuleRoots) {
		return RenamePreview{}, ErrInvalidRenameRequest
	}
	pageSize := request.PageSize
	if pageSize == 0 {
		pageSize = DefaultRenameEditPage
	}
	readTimeout := request.ReadTimeout
	if readTimeout == 0 {
		readTimeout = defaultLSPReadTimeout
	}
	if readTimeout < time.Second || readTimeout > maxLSPTimeout {
		return RenamePreview{}, ErrInvalidRenameRequest
	}
	session, err := StartSession(ctx, SessionConfig{
		Snapshot: request.Snapshot, Profile: request.Profile, Policy: request.Policy,
		StartupTimeout: request.StartupTimeout, ReadTimeout: readTimeout, RenameTimeout: request.RenameTimeout, ShutdownTimeout: request.ShutdownTimeout,
	})
	if err != nil {
		return RenamePreview{}, err
	}
	prepared, rawEdit, renameErr := session.Rename(ctx, request.Source, request.NewName)
	closeErr := session.Close()
	if renameErr != nil {
		return RenamePreview{}, errors.Join(renameErr, closeErr)
	}
	if closeErr != nil {
		return RenamePreview{}, closeErr
	}
	if len(rawEdit) > maxLSPMessageBytes {
		return RenamePreview{}, ErrRenameUnsupported
	}

	readCtx, cancelRead := context.WithTimeout(ctx, readTimeout)
	defer cancelRead()
	workspace, err := validateWorkspaceSnapshot(readCtx, request.Snapshot, request.Profile)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return RenamePreview{}, ErrLSPTimeout
		}
		return RenamePreview{}, err
	}
	defer workspace.close()
	sourceBytes, err := workspace.readSource(readCtx, request.Source.Path, request.Source.ContentHash, request.Profile.MaxFileBytes)
	if err != nil {
		return RenamePreview{}, err
	}
	if err := validateSourceForScope(request.Source, sourceBytes, request.Identity.Scope); err != nil {
		clear(sourceBytes)
		return RenamePreview{}, err
	}
	if !strings.EqualFold(filepath.Ext(request.Source.Path), ".go") {
		clear(sourceBytes)
		return RenamePreview{}, ErrRenameUnsupported
	}
	clear(sourceBytes)
	positionEncoding := session.encoding
	if positionEncoding == "" {
		return RenamePreview{}, ErrLSPNotReady
	}
	edits, err := validateWorkspaceEdit(readCtx, rawEdit, workspace, positionEncoding, prepared.Placeholder, request.NewName)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return RenamePreview{}, ErrLSPTimeout
		}
		return RenamePreview{}, err
	}
	if len(edits) > MaxRenameResultEdits {
		return RenamePreview{}, sqlite.ErrAdmissionResourceLimit
	}
	wireEdits := make([]json.RawMessage, len(edits))
	var serializedBytes int64
	for index, edit := range edits {
		wireEdits[index] = edit.wire
		serializedBytes += int64(len(edit.wire))
		if serializedBytes > maxRenamePreviewBytes {
			return RenamePreview{}, sqlite.ErrAdmissionResourceLimit
		}
	}
	ref, err := provider.results.Publish(ctx, request.Identity, wireEdits)
	if err != nil {
		return RenamePreview{}, err
	}
	page, err := provider.results.Page(ctx, request.Identity, ref, provider.cursors, "", pageSize)
	if err != nil {
		return RenamePreview{}, errors.Join(err, provider.results.Remove(context.Background(), ref))
	}
	return RenamePreview{
		ResultRef: ref, Edits: page.Edits, NextCursor: page.NextCursor, Position: page.Position,
		HasMore: page.HasMore, Prepared: prepared, Warnings: append([]string(nil), prepared.Warnings...), ValidationRequired: true, Applied: false,
		SourceMutationCount: 0,
	}, nil
}

// Continue reads only the immutable result spool. It has no access to a
// workspace or provider process, so a continuation cannot invoke gopls again.
func (provider *RenameProvider) Continue(ctx context.Context, identity ResultIdentity, ref ResultRef, cursor string, pageSize int) (RenamePreview, error) {
	if provider == nil || provider.results == nil || provider.cursors == nil || ctx == nil || pageSize < 1 || pageSize > maxRenamePageSize {
		return RenamePreview{}, ErrInvalidRenameRequest
	}
	page, err := provider.results.Page(ctx, identity, ref, provider.cursors, cursor, pageSize)
	if err != nil {
		return RenamePreview{}, err
	}
	return RenamePreview{
		ResultRef: ref, Edits: page.Edits, NextCursor: page.NextCursor, Position: page.Position,
		HasMore: page.HasMore, ValidationRequired: true, Applied: false, SourceMutationCount: 0,
	}, nil
}

func validateWorkspaceEdit(ctx context.Context, raw json.RawMessage, workspace *validatedWorkspace, encoding PositionEncoding, oldName, newName string) ([]validatedRenameEdit, error) {
	if ctx == nil || workspace == nil || len(raw) == 0 || len(raw) > maxLSPMessageBytes || !json.Valid(raw) {
		return nil, ErrLSPProtocol
	}
	if _, err := contracts.DecodeJSON(raw); err != nil {
		return nil, ErrRenameUnsupported
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []validatedRenameEdit{}, nil
	}
	var edit workspaceEdit
	if decodeStrictJSON(raw, &edit) != nil {
		return nil, ErrRenameUnsupported
	}
	if len(edit.ChangeAnnotations) > 0 || (edit.Changes != nil && edit.DocumentChanges != nil) {
		return nil, ErrRenameUnsupported
	}
	collected := make([]renameCandidate, 0)
	if edit.Changes != nil {
		uris := make([]string, 0, len(edit.Changes))
		for uri := range edit.Changes {
			uris = append(uris, uri)
		}
		sort.Strings(uris)
		for _, uri := range uris {
			fileEdits := edit.Changes[uri]
			if fileEdits == nil {
				return nil, ErrRenameUnsupported
			}
			for _, textEdit := range fileEdits {
				collected = append(collected, renameCandidate{uri: uri, text: textEdit})
			}
		}
	}
	if edit.DocumentChanges != nil {
		for _, rawChange := range edit.DocumentChanges {
			var documentEdit lspTextDocumentEdit
			if decodeStrictJSON(rawChange, &documentEdit) != nil || documentEdit.TextDocument.URI == "" || documentEdit.TextDocument.Version == nil || documentEdit.Edits == nil {
				return nil, ErrRenameUnsupported
			}
			version := bytes.TrimSpace(documentEdit.TextDocument.Version)
			if !bytes.Equal(version, []byte("null")) {
				var numeric int64
				if json.Unmarshal(version, &numeric) != nil || numeric != 1 {
					return nil, ErrCapturedWorkspace
				}
			}
			for _, textEdit := range documentEdit.Edits {
				collected = append(collected, renameCandidate{uri: documentEdit.TextDocument.URI, text: textEdit})
			}
		}
	}
	if len(collected) > MaxRenameResultEdits {
		return nil, sqlite.ErrAdmissionResourceLimit
	}
	contents := make(map[string][]byte)
	defer func() {
		for _, content := range contents {
			clear(content)
		}
	}()
	validated := make([]validatedRenameEdit, 0, len(collected))
	for _, item := range collected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := fileURIToRelative(workspace.rootPath, item.uri, workspace.files)
		if err != nil {
			return nil, err
		}
		if !sourcePathInModuleRoots(path, workspace.moduleRoots) {
			return nil, ErrCapturedWorkspace
		}
		if !strings.EqualFold(filepath.Ext(path), ".go") {
			return nil, ErrRenameUnsupported
		}
		content := contents[path]
		if content == nil {
			captured := workspace.files[path]
			content, err = workspace.readSource(ctx, path, captured.ContentHash, 2*1024*1024)
			if err != nil {
				return nil, err
			}
			contents[path] = content
		}
		if !utf8.ValidString(item.text.NewText) || item.text.AnnotationID != "" || item.text.NewText != newName || newName == "" || oldName == "" {
			return nil, ErrRenameUnsupported
		}
		start, err := PositionToByteOffset(content, item.text.Range.Start, encoding)
		if err != nil {
			return nil, err
		}
		end, err := PositionToByteOffset(content, item.text.Range.End, encoding)
		if err != nil || end <= start {
			return nil, ErrInvalidPosition
		}
		byteRange := model.ByteRange{StartByte: start, EndByte: end}
		if byteRange.ValidateAgainst(content) != nil {
			return nil, ErrInvalidPosition
		}
		if string(content[start:end]) != oldName {
			return nil, ErrRenameConflict
		}
		validatedEdit := RenameEdit{Path: path, ContentHash: workspace.files[path].ContentHash, Version: 1, Range: byteRange, NewText: item.text.NewText}
		wire, marshalErr := json.Marshal(validatedEdit)
		if marshalErr != nil {
			return nil, ErrLSPProtocol
		}
		validated = append(validated, validatedRenameEdit{edit: validatedEdit, wire: wire})
	}
	sort.Slice(validated, func(i, j int) bool {
		left, right := validated[i].edit, validated[j].edit
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Range.StartByte != right.Range.StartByte {
			return left.Range.StartByte < right.Range.StartByte
		}
		if left.Range.EndByte != right.Range.EndByte {
			return left.Range.EndByte < right.Range.EndByte
		}
		return left.NewText < right.NewText
	})
	for index := 1; index < len(validated); index++ {
		previous, current := validated[index-1].edit, validated[index].edit
		if previous.Path != current.Path {
			continue
		}
		if current.Range.StartByte < previous.Range.EndByte || current.Range.StartByte == previous.Range.StartByte {
			return nil, ErrRenameConflict
		}
	}
	var bytesTotal int64
	for _, item := range validated {
		bytesTotal += int64(len(item.wire))
		if bytesTotal > maxRenamePreviewBytes {
			return nil, sqlite.ErrAdmissionResourceLimit
		}
	}
	return validated, nil
}

func tokenIsIdentifier(value string) bool {
	return value != "_" && utf8.ValidString(value) && token.IsIdentifier(value) && !token.Lookup(value).IsKeyword()
}

func sourcePathInModuleRoots(path string, roots []string) bool {
	if model.ValidateRelativePath(path) != nil {
		return false
	}
	for _, root := range roots {
		if root == "." || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func assessRenameSite(source []byte, path string, offset int64) ([]string, bool) {
	warnings := []string{"RUNTIME_STRING_LOOKUP_REVIEW_REQUIRED"}
	fileSet := token.NewFileSet()
	parsed, _ := parser.ParseFile(fileSet, path, source, parser.AllErrors)
	if parsed == nil {
		return warnings, false
	}
	byteOffset := func(position token.Pos) int64 {
		file := fileSet.File(position)
		if file == nil {
			return -1
		}
		return int64(file.Offset(position))
	}
	if start, end := byteOffset(parsed.Name.Pos()), byteOffset(parsed.Name.End()); start >= 0 && offset >= start && offset < end {
		return warnings, true
	}
	implicitSerializationChange := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		field, ok := node.(*ast.Field)
		if !ok {
			return true
		}
		for _, name := range field.Names {
			start, end := byteOffset(name.Pos()), byteOffset(name.End())
			if start < 0 || offset < start || offset >= end || !token.IsExported(name.Name) {
				continue
			}
			if field.Tag == nil {
				implicitSerializationChange = true
				continue
			}
			rawTag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				implicitSerializationChange = true
				continue
			}
			jsonTag, exists := reflect.StructTag(rawTag).Lookup("json")
			jsonName := strings.SplitN(jsonTag, ",", 2)[0]
			if !exists || jsonName == "" {
				implicitSerializationChange = true
			}
		}
		return true
	})
	if implicitSerializationChange {
		warnings = append(warnings, "IMPLICIT_SERIALIZATION_NAME_CHANGE")
	}
	return warnings, false
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err == nil {
		return ErrLSPProtocol
	} else if !errors.Is(err, io.EOF) {
		return ErrLSPProtocol
	}
	return nil
}

func sameScope(left, right model.ScopeVector) bool {
	leftDigest, leftErr := left.ComputeDigest()
	rightDigest, rightErr := right.ComputeDigest()
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest && left.CatalogIncarnation == right.CatalogIncarnation
}

var ErrInvalidRenameRequest = errors.New("rename preview request is invalid")
