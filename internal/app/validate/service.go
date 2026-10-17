// Package validate provides bounded, full-workspace freshness checks for edit preparation.
package validate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

const maxStabilityRetries = 2

var (
	ErrInvalidRequest        = errors.New("invalid workspace validation request")
	ErrFileChangedDuringRead = errors.New("file changed while being captured")
)

type Workspace interface {
	Scan(context.Context) (inventory.Snapshot, error)
	CaptureFile(context.Context, string) (FileCapture, error)
}

type FileCapture struct {
	ContentHash model.Hash
	ByteLength  int64
	FileInfo    os.FileInfo
}

type ScopeReader interface {
	ReadScopeObservation(context.Context) (snapshot.ScopeObservation, error)
}

type Request struct {
	Expected            model.CaptureManifest
	ExpectedComplete    bool
	ExpectedScopeVector model.ScopeVector
	SourcePreconditions []snapshot.SourcePrecondition
}

type Service struct {
	workspace Workspace
	scope     ScopeReader
}

func NewService(workspace Workspace, scope ScopeReader) (*Service, error) {
	if workspace == nil || scope == nil {
		return nil, fmt.Errorf("workspace and scope reader are required: %w", ErrInvalidRequest)
	}
	return &Service{workspace: workspace, scope: scope}, nil
}

// LocalWorkspace uses the existing Git inventory and verified-root file opener.
// Every read checks the complete content bytes; Git status and stat hints are
// never freshness evidence.
type LocalWorkspace struct {
	root    *runtime.Root
	options inventory.Options
	policy  *policy.Policy
}

func NewLocalWorkspace(root *runtime.Root, options inventory.Options) (*LocalWorkspace, error) {
	if root == nil || options.Root == "" || options.Policy == nil || options.GitExecutable == "" {
		return nil, fmt.Errorf("verified root, inventory root, Git executable, and policy are required: %w", ErrInvalidRequest)
	}
	finalPath, err := root.FinalPath()
	if err != nil {
		return nil, fmt.Errorf("resolve verified root: %w", ErrInvalidRequest)
	}
	rootInfo, err := os.Stat(finalPath)
	if err != nil {
		return nil, fmt.Errorf("stat verified root: %w", ErrInvalidRequest)
	}
	optionsInfo, err := os.Stat(options.Root)
	if err != nil || !os.SameFile(rootInfo, optionsInfo) {
		return nil, fmt.Errorf("inventory and secure-open roots must identify the same directory: %w", ErrInvalidRequest)
	}
	rootCaseMode, err := root.CaseMode()
	if err != nil || options.CaseMode != rootCaseMode {
		return nil, fmt.Errorf("inventory case mode must match the verified root: %w", ErrInvalidRequest)
	}
	return &LocalWorkspace{root: root, options: options, policy: options.Policy}, nil
}

func (workspace *LocalWorkspace) Scan(ctx context.Context) (inventory.Snapshot, error) {
	if workspace == nil {
		return inventory.Snapshot{}, ErrInvalidRequest
	}
	return inventory.Scan(ctx, workspace.options)
}

func (workspace *LocalWorkspace) CaptureFile(ctx context.Context, relative string) (result FileCapture, resultErr error) {
	if workspace == nil || ctx == nil {
		return FileCapture{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return FileCapture{}, err
	}
	decision, err := workspace.policy.CheckPath(relative, false)
	if err != nil {
		return FileCapture{}, err
	}
	if decision.Denied {
		return FileCapture{}, runtime.ErrPathDenied
	}
	if !workspace.policy.AllowsText(relative) {
		return FileCapture{}, policy.ErrUnsupportedTextType
	}
	file, err := workspace.root.OpenRegularFile(relative)
	if err != nil {
		return FileCapture{}, err
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil && closeErr != nil {
			result = FileCapture{}
			resultErr = runtime.ErrOpenFailed
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return FileCapture{}, runtime.ErrOpenFailed
	}
	if !before.Mode().IsRegular() {
		return FileCapture{}, runtime.ErrNotRegularFile
	}
	if before.Size() < 0 || before.Size() > policy.MaxFileBytes {
		return FileCapture{}, policy.ErrContentTooLarge
	}
	content, err := readBoundedContext(ctx, file, policy.MaxFileBytes)
	if err != nil {
		clear(content)
		return FileCapture{}, err
	}
	defer clear(content)
	if _, err := workspace.policy.ScanContent(content); err != nil {
		return FileCapture{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return FileCapture{}, runtime.ErrOpenFailed
	}
	if !sameCapturedVersion(before, after, int64(len(content))) {
		return FileCapture{}, ErrFileChangedDuringRead
	}
	if err := ctx.Err(); err != nil {
		return FileCapture{}, err
	}
	return FileCapture{ContentHash: model.SHA256(content), ByteLength: int64(len(content)), FileInfo: after}, nil
}

func (service *Service) Validate(ctx context.Context, request Request) (snapshot.ValidationResult, error) {
	if service == nil || service.workspace == nil || service.scope == nil || ctx == nil {
		return snapshot.ValidationResult{}, fmt.Errorf("service and context are required: %w", ErrInvalidRequest)
	}
	if err := request.Expected.Validate(); err != nil {
		return snapshot.ValidationResult{}, fmt.Errorf("expected manifest: %w: %v", ErrInvalidRequest, err)
	}
	if err := request.ExpectedScopeVector.Validate(); err != nil {
		return snapshot.ValidationResult{}, fmt.Errorf("expected scope vector: %w: %v", ErrInvalidRequest, err)
	}
	if err := snapshot.ValidateBaseline(request.Expected, request.ExpectedComplete, request.ExpectedScopeVector, request.SourcePreconditions); err != nil {
		return snapshot.ValidationResult{}, fmt.Errorf("validate request baseline: %w: %v", ErrInvalidRequest, err)
	}
	for attempt := 0; attempt <= maxStabilityRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		beforeScope, err := service.scope.ReadScopeObservation(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return snapshot.UnknownValidation(nowUTC(), freshnessReason(ctxErr)), nil
			}
			return snapshot.UnknownValidation(nowUTC(), "SCOPE_VECTOR_UNAVAILABLE"), nil
		}
		if err := beforeScope.Validate(); err != nil {
			return snapshot.UnknownValidation(nowUTC(), "CONFIGURATION_UNAVAILABLE"), nil
		}
		firstInventory, err := service.workspace.Scan(ctx)
		if err != nil {
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		firstHashes, err := hashWorkspace(ctx, service.workspace, firstInventory)
		if err != nil {
			if errors.Is(err, ErrFileChangedDuringRead) && attempt < maxStabilityRetries {
				continue
			}
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		secondInventory, err := service.workspace.Scan(ctx)
		if err != nil {
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		secondHashes, err := hashWorkspace(ctx, service.workspace, secondInventory)
		if err != nil {
			if errors.Is(err, ErrFileChangedDuringRead) && attempt < maxStabilityRetries {
				continue
			}
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		finalInventory, err := service.workspace.Scan(ctx)
		if err != nil {
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		afterScope, err := service.scope.ReadScopeObservation(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return snapshot.UnknownValidation(nowUTC(), freshnessReason(ctxErr)), nil
			}
			return snapshot.UnknownValidation(nowUTC(), "SCOPE_VECTOR_UNAVAILABLE"), nil
		}
		if err := ctx.Err(); err != nil {
			return snapshot.UnknownValidation(nowUTC(), freshnessReason(err)), nil
		}
		if err := afterScope.Validate(); err != nil {
			return snapshot.UnknownValidation(nowUTC(), "CONFIGURATION_UNAVAILABLE"), nil
		}
		if !sameInventory(firstInventory, secondInventory) || !sameInventory(secondInventory, finalInventory) || !sameHashes(firstHashes, secondHashes) || !sameScopeObservation(beforeScope, afterScope) {
			if attempt < maxStabilityRetries {
				continue
			}
			return snapshot.UnknownValidation(nowUTC(), "WORKSPACE_CHANGED_DURING_VALIDATION"), nil
		}
		files := make([]snapshot.WorkspaceFile, 0, len(secondHashes))
		for path, capture := range secondHashes {
			files = append(files, snapshot.WorkspaceFile{Path: path, ContentHash: capture.ContentHash, ByteLength: capture.ByteLength})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		return snapshot.CompareValidation(snapshot.ValidationInput{
			Expected: request.Expected, ExpectedComplete: request.ExpectedComplete,
			ExpectedScopeVector: request.ExpectedScopeVector,
			ObservedFiles:       files, ObservedComplete: true, ObservedScope: afterScope,
			SourcePreconditions: request.SourcePreconditions, CheckedAt: nowUTC(),
		})
	}
	return snapshot.UnknownValidation(nowUTC(), "WORKSPACE_CHANGED_DURING_VALIDATION"), nil
}

func hashWorkspace(ctx context.Context, workspace Workspace, observed inventory.Snapshot) (map[string]FileCapture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	coverage := observed.Coverage
	knownMissingOnly := coverage.State == inventory.CoveragePartial && coverage.Missing > 0 &&
		coverage.Inaccessible == 0 && coverage.Sparse == 0 && coverage.Unmerged == 0 &&
		coverage.Symlinks == 0 && coverage.NestedRepositories == 0 &&
		coverage.UnsupportedPath == 0 && coverage.UnsupportedPathEncoding == 0
	if (coverage.State != inventory.CoverageComplete && !knownMissingOnly) || !coverage.CaseSemanticsKnown || len(observed.Boundaries) != 0 {
		return nil, errIncompleteWorkspace
	}
	hashes := make(map[string]FileCapture, len(observed.Files))
	for _, file := range observed.Files {
		if file.State == inventory.FileMissing {
			continue
		}
		if file.State != inventory.FilePresent || !file.TextSearchEligible {
			return nil, errIncompleteWorkspace
		}
		capture, err := workspace.CaptureFile(ctx, file.Path)
		if err != nil {
			return nil, err
		}
		hashes[file.Path] = capture
	}
	return hashes, nil
}

var errIncompleteWorkspace = errors.New("workspace inventory cannot be fully captured under current policy")

func sameInventory(left, right inventory.Snapshot) bool {
	if left.MembershipDigest != right.MembershipDigest || left.Coverage != right.Coverage || len(left.Files) != len(right.Files) || len(left.Boundaries) != len(right.Boundaries) {
		return false
	}
	for index := range left.Files {
		if left.Files[index] != right.Files[index] {
			return false
		}
	}
	for index := range left.Boundaries {
		if left.Boundaries[index] != right.Boundaries[index] {
			return false
		}
	}
	return true
}

func sameHashes(left, right map[string]FileCapture) bool {
	if len(left) != len(right) {
		return false
	}
	for path, capture := range left {
		other, exists := right[path]
		if !exists || capture.ContentHash != other.ContentHash || capture.ByteLength != other.ByteLength || !sameFileObservation(capture.FileInfo, other.FileInfo) {
			return false
		}
	}
	return true
}

func sameFileObservation(left, right os.FileInfo) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return os.SameFile(left, right) && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func sameScopeObservation(left, right snapshot.ScopeObservation) bool {
	return left.Complete && right.Complete && left.Vector.VectorDigest != "" &&
		left.Vector.VectorDigest == right.Vector.VectorDigest &&
		left.Vector.CatalogIncarnation == right.Vector.CatalogIncarnation &&
		left.ProviderDigest == right.ProviderDigest &&
		sameHashSlices(left.DependencyInputDigests, right.DependencyInputDigests)
}

func sameHashSlices(left, right []model.Hash) bool {
	if len(left) != len(right) {
		return false
	}
	leftSorted := append([]model.Hash(nil), left...)
	rightSorted := append([]model.Hash(nil), right...)
	sort.Slice(leftSorted, func(i, j int) bool { return leftSorted[i] < leftSorted[j] })
	sort.Slice(rightSorted, func(i, j int) bool { return rightSorted[i] < rightSorted[j] })
	for index := range leftSorted {
		if leftSorted[index] != rightSorted[index] {
			return false
		}
	}
	return true
}

func sameCapturedVersion(before, after os.FileInfo, bytesRead int64) bool {
	return os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && bytesRead == before.Size()
}

func readBoundedContext(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	buffer := make([]byte, 32*1024)
	defer clear(buffer)
	content := make([]byte, 0, limit)
	for {
		if err := ctx.Err(); err != nil {
			return content, err
		}
		read, err := reader.Read(buffer)
		if read > 0 {
			if int64(len(content))+int64(read) > limit {
				return content, policy.ErrContentTooLarge
			}
			content = append(content, buffer[:read]...)
		}
		if errors.Is(err, io.EOF) {
			return content, nil
		}
		if err != nil {
			return content, runtime.ErrOpenFailed
		}
		if read == 0 {
			return content, io.ErrNoProgress
		}
	}
}

func freshnessReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "FRESHNESS_DEADLINE"
	case errors.Is(err, context.Canceled):
		return "FRESHNESS_CANCELLED"
	case errors.Is(err, ErrFileChangedDuringRead):
		return "FILE_CHANGED_DURING_CAPTURE"
	case errors.Is(err, errIncompleteWorkspace):
		return "WORKSPACE_COVERAGE_INCOMPLETE"
	case errors.Is(err, policy.ErrContentTooLarge):
		return "CONTENT_POLICY_LIMIT"
	case errors.Is(err, policy.ErrUnsupportedTextType):
		return "CONTENT_POLICY_UNSUPPORTED"
	case errors.Is(err, policy.ErrSecretDetected), errors.Is(err, policy.ErrBinaryContent), errors.Is(err, policy.ErrInvalidUTF8):
		return "CONTENT_POLICY_BLOCKED"
	case errors.Is(err, runtime.ErrPathDenied), errors.Is(err, runtime.ErrNotRegularFile), errors.Is(err, runtime.ErrReparsePoint):
		return "FILE_POLICY_BLOCKED"
	case errors.Is(err, runtime.ErrPathEscape):
		return "FILE_OUTSIDE_VERIFIED_ROOT"
	case errors.Is(err, runtime.ErrOpenFailed):
		return "FILE_READ_FAILED"
	case errors.Is(err, inventory.ErrGitCommand), errors.Is(err, inventory.ErrInvalidOptions):
		return "INVENTORY_FAILED"
	default:
		return "FRESHNESS_UNKNOWN"
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
