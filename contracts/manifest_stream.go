package contracts

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	defaultManifestByteLimit  = int64(34359738368)
	defaultManifestEntryLimit = int64(1_000_000)
	manifestRunEntries        = 4096
	manifestMergeFanIn        = 256
	manifestScratchBuffer     = 64 * 1024
	maxManifestStringRunes    = 4096
)

var (
	ErrManifestStream           = errors.New("invalid streaming capture manifest")
	ErrManifestTooLarge         = errors.New("capture manifest exceeds its byte limit")
	ErrManifestNotCanonical     = errors.New("capture manifest is not canonical MCE-MANIFEST-JSON1")
	ErrManifestSpoolNotAdmitted = errors.New("capture manifest spool is not admitted")
)

// ManifestSpoolLease is an admission-backed, private scratch reservation.
// ReserveGrowth is called before each write that can grow an on-disk spool.
// Release must relinquish this operation's reservation, even after failure.
type ManifestSpoolLease interface {
	Root() string
	ReserveGrowth(ctx context.Context, bytes int64) error
	Release() error
}

type CaptureManifestStreamOptions struct {
	MaxBytes         int64
	MaxFiles         int64
	MaxDependencies  int64
	RequireCanonical bool
}

// NormalizeCaptureManifestJSON1 parses one bounded manifest into private,
// admitted disk spools, external-sorts files and dependency digests in runs of
// at most 4096 entries, and emits canonical bytes only after complete checks.
// The destination should itself be a transactional staging writer.
func NormalizeCaptureManifestJSON1(ctx context.Context, src io.Reader, dst io.Writer, lease ManifestSpoolLease, options CaptureManifestStreamOptions) (summary model.ManifestWriteSummary, resultErr error) {
	if lease == nil {
		return summary, ErrManifestSpoolNotAdmitted
	}
	defer func() {
		if err := lease.Release(); resultErr == nil && err != nil {
			resultErr = fmt.Errorf("release manifest spool admission: %w", err)
		}
	}()
	if ctx == nil || src == nil || dst == nil {
		return summary, fmt.Errorf("%w: context, reader, and writer are required", ErrManifestStream)
	}
	options, err := normalizeManifestStreamOptions(options)
	if err != nil {
		return summary, err
	}
	root, err := verifiedScratchRoot(lease.Root())
	if err != nil {
		return summary, err
	}
	tempRoot, err := os.MkdirTemp(root, ".mce4d-manifest-")
	if err != nil {
		return summary, fmt.Errorf("create owned manifest spool: %w", err)
	}
	if filepath.Dir(tempRoot) != root {
		return summary, errors.Join(ErrManifestSpoolNotAdmitted, os.RemoveAll(tempRoot))
	}
	defer func() {
		if err := os.RemoveAll(tempRoot); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove owned manifest spool: %w", err))
		}
	}()

	rawPath := filepath.Join(tempRoot, "raw.input")
	rawSize, err := spoolInput(ctx, src, rawPath, lease, options.MaxBytes)
	if err != nil {
		return summary, err
	}
	rawFile, err := os.Open(rawPath)
	if err != nil {
		return summary, err
	}
	if err := validateManifestJSONLexemes(ctx, rawFile); err != nil {
		_ = rawFile.Close()
		return summary, err
	}
	if _, err := rawFile.Seek(0, io.SeekStart); err != nil {
		_ = rawFile.Close()
		return summary, err
	}
	parsed, err := parseManifestToSpools(ctx, rawFile, tempRoot, lease, options)
	closeErr := rawFile.Close()
	if err != nil {
		return summary, err
	}
	if closeErr != nil {
		return summary, closeErr
	}
	filesPath, err := mergeManifestFileRuns(ctx, parsed.fileRuns, filepath.Join(tempRoot, "files.sorted"), lease)
	if err != nil {
		return summary, err
	}
	dependencyPath, err := mergeManifestHashRuns(ctx, parsed.dependencyRuns, filepath.Join(tempRoot, "dependencies.sorted"), lease)
	if err != nil {
		return summary, err
	}
	files := &manifestFileSpoolSource{path: filesPath}
	dependencies := &manifestHashSpoolSource{path: dependencyPath}
	defer func() {
		if err := files.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close sorted file spool: %w", err))
		}
		if err := dependencies.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close sorted dependency spool: %w", err))
		}
	}()
	stream := model.CaptureManifestStream{
		Header: parsed.header, FileCount: parsed.fileCount, Files: files,
		DependencyCount: parsed.dependencyCount, Dependencies: dependencies,
	}
	canonicalPath := filepath.Join(tempRoot, "canonical.output")
	canonical, err := createPrivateSpool(canonicalPath)
	if err != nil {
		return summary, err
	}
	boundedOutput := &manifestOutputLimitWriter{
		writer: &admittedFileWriter{ctx: ctx, lease: lease, file: canonical},
		limit:  options.MaxBytes,
	}
	buffered := bufio.NewWriterSize(boundedOutput, manifestScratchBuffer)
	summary, err = model.WriteCaptureManifestJSON1Stream(buffered, stream)
	if err == nil {
		err = buffered.Flush()
	}
	if err == nil {
		err = canonical.Sync()
	}
	closeErr = canonical.Close()
	if err != nil {
		return model.ManifestWriteSummary{}, fmt.Errorf("write normalized manifest spool: %w", err)
	}
	if closeErr != nil {
		return model.ManifestWriteSummary{}, closeErr
	}
	if summary.ByteLength > options.MaxBytes {
		return model.ManifestWriteSummary{}, ErrManifestTooLarge
	}
	if options.RequireCanonical {
		equal, compareErr := spoolFilesEqual(ctx, rawPath, canonicalPath)
		if compareErr != nil {
			return model.ManifestWriteSummary{}, compareErr
		}
		if !equal {
			return model.ManifestWriteSummary{}, ErrManifestNotCanonical
		}
	}
	output, err := os.Open(canonicalPath)
	if err != nil {
		return model.ManifestWriteSummary{}, err
	}
	if _, err := io.Copy(dst, contextReader{ctx: ctx, reader: output}); err != nil {
		_ = output.Close()
		return model.ManifestWriteSummary{}, err
	}
	closeErr = output.Close()
	if closeErr != nil {
		return model.ManifestWriteSummary{}, closeErr
	}
	if summary.ByteLength <= 0 || summary.ByteLength > options.MaxBytes || rawSize < 1 {
		return model.ManifestWriteSummary{}, ErrManifestTooLarge
	}
	return summary, nil
}

// ValidateCanonicalCaptureManifestJSON1 verifies an artifact that is already
// persisted as MCE-MANIFEST-JSON1. Key/escape/order differences fail closed.
func ValidateCanonicalCaptureManifestJSON1(ctx context.Context, src io.Reader, dst io.Writer, lease ManifestSpoolLease, options CaptureManifestStreamOptions) (model.ManifestWriteSummary, error) {
	options.RequireCanonical = true
	return NormalizeCaptureManifestJSON1(ctx, src, dst, lease, options)
}

func normalizeManifestStreamOptions(options CaptureManifestStreamOptions) (CaptureManifestStreamOptions, error) {
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultManifestByteLimit
	}
	if options.MaxFiles == 0 {
		options.MaxFiles = defaultManifestEntryLimit
	}
	if options.MaxDependencies == 0 {
		options.MaxDependencies = defaultManifestEntryLimit
	}
	if options.MaxBytes < 1 || options.MaxBytes > defaultManifestByteLimit || options.MaxFiles < 0 || options.MaxFiles > defaultManifestEntryLimit || options.MaxDependencies < 0 || options.MaxDependencies > defaultManifestEntryLimit {
		return options, fmt.Errorf("%w: manifest limits exceed the closed schema", ErrManifestStream)
	}
	return options, nil
}

func verifiedScratchRoot(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", ErrManifestSpoolNotAdmitted
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", ErrManifestSpoolNotAdmitted
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrManifestSpoolNotAdmitted
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(absolute) {
		return "", ErrManifestSpoolNotAdmitted
	}
	return absolute, nil
}

func spoolInput(ctx context.Context, src io.Reader, path string, lease ManifestSpoolLease, maxBytes int64) (int64, error) {
	file, err := createPrivateSpool(path)
	if err != nil {
		return 0, err
	}
	limited := io.LimitReader(contextReader{ctx: ctx, reader: src}, maxBytes+1)
	written, copyErr := io.Copy(&admittedFileWriter{ctx: ctx, lease: lease, file: file}, limited)
	if copyErr == nil && written > maxBytes {
		copyErr = ErrManifestTooLarge
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	return written, nil
}

func createPrivateSpool(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrManifestSpoolNotAdmitted
	}
	return file, nil
}

type admittedFileWriter struct {
	ctx   context.Context
	lease ManifestSpoolLease
	file  io.Writer
}

func (writer *admittedFileWriter) Write(value []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if len(value) > 0 {
		if err := writer.lease.ReserveGrowth(writer.ctx, int64(len(value))); err != nil {
			return 0, err
		}
	}
	return writer.file.Write(value)
}

type manifestOutputLimitWriter struct {
	writer  io.Writer
	limit   int64
	written int64
}

func (writer *manifestOutputLimitWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > writer.limit-writer.written {
		return 0, ErrManifestTooLarge
	}
	written, err := writer.writer.Write(value)
	writer.written += int64(written)
	return written, err
}

type parsedManifestSpools struct {
	header          model.CaptureManifestHeader
	fileCount       int64
	dependencyCount int64
	fileRuns        []string
	dependencyRuns  []string
}

func parseManifestToSpools(ctx context.Context, raw io.Reader, spoolRoot string, lease ManifestSpoolLease, options CaptureManifestStreamOptions) (parsedManifestSpools, error) {
	decoder := json.NewDecoder(bufio.NewReaderSize(raw, manifestScratchBuffer))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return parsedManifestSpools{}, fmt.Errorf("%w: root must be an object", ErrManifestStream)
	}
	parsed := parsedManifestSpools{}
	seen := make(map[string]bool, 8)
	fileChunk := make([]model.ManifestFile, 0, manifestRunEntries)
	hashChunk := make([]model.Hash, 0, manifestRunEntries)
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return parsedManifestSpools{}, err
		}
		keyValue, err := decoder.Token()
		if err != nil {
			return parsedManifestSpools{}, fmt.Errorf("%w: invalid object key", ErrManifestStream)
		}
		key, ok := keyValue.(string)
		if !ok || seen[key] {
			return parsedManifestSpools{}, fmt.Errorf("%w: invalid or duplicate root key", ErrManifestStream)
		}
		seen[key] = true
		switch key {
		case "schema_version":
			parsed.header.SchemaVersion, err = readManifestInteger(decoder)
		case "worktree_id":
			var value string
			value, err = readManifestString(decoder)
			parsed.header.WorktreeID = model.UUID(value)
		case "profile_id":
			parsed.header.ProfileID, err = readManifestHash(decoder)
		case "policy_digest":
			parsed.header.PolicyDigest, err = readManifestHash(decoder)
		case "provider_digest":
			parsed.header.ProviderDigest, err = readManifestHash(decoder)
		case "snapshot_digest":
			parsed.header.SnapshotDigest, err = readManifestHash(decoder)
		case "files":
			err = readManifestFileArray(ctx, decoder, &parsed, &fileChunk, spoolRoot, lease, options)
		case "dependency_input_digests":
			err = readManifestHashArray(ctx, decoder, &parsed, &hashChunk, spoolRoot, lease, options)
		default:
			return parsedManifestSpools{}, fmt.Errorf("%w: unknown root field", ErrManifestStream)
		}
		if err != nil {
			return parsedManifestSpools{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return parsedManifestSpools{}, fmt.Errorf("%w: root object is incomplete", ErrManifestStream)
	}
	if len(seen) != 8 {
		return parsedManifestSpools{}, fmt.Errorf("%w: required root fields are missing", ErrManifestStream)
	}
	if err := flushManifestFileRun(ctx, fileChunk, &parsed, spoolRoot, lease); err != nil {
		return parsedManifestSpools{}, err
	}
	if err := flushManifestHashRun(ctx, hashChunk, &parsed, spoolRoot, lease); err != nil {
		return parsedManifestSpools{}, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			_ = token
		}
		return parsedManifestSpools{}, fmt.Errorf("%w: trailing JSON value or bytes", ErrManifestStream)
	}
	return parsed, nil
}

func readManifestFileArray(ctx context.Context, decoder *json.Decoder, parsed *parsedManifestSpools, chunk *[]model.ManifestFile, spoolRoot string, lease ManifestSpoolLease, options CaptureManifestStreamOptions) error {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return fmt.Errorf("%w: files must be an array", ErrManifestStream)
	}
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parsed.fileCount >= options.MaxFiles {
			return fmt.Errorf("%w: file entry limit exceeded", ErrManifestStream)
		}
		file, err := readManifestFile(decoder)
		if err != nil {
			return err
		}
		if err := model.ValidateCaptureManifestFile(file); err != nil {
			return fmt.Errorf("%w: invalid file entry: %v", ErrManifestStream, err)
		}
		*chunk = append(*chunk, file)
		parsed.fileCount++
		if len(*chunk) == manifestRunEntries {
			if err := flushManifestFileRun(ctx, *chunk, parsed, spoolRoot, lease); err != nil {
				return err
			}
			*chunk = make([]model.ManifestFile, 0, manifestRunEntries)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return fmt.Errorf("%w: files array is incomplete", ErrManifestStream)
	}
	return nil
}

func readManifestFile(decoder *json.Decoder) (model.ManifestFile, error) {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return model.ManifestFile{}, fmt.Errorf("%w: file entry must be an object", ErrManifestStream)
	}
	var file model.ManifestFile
	seen := make(map[string]bool, 5)
	for decoder.More() {
		keyValue, err := decoder.Token()
		if err != nil {
			return model.ManifestFile{}, fmt.Errorf("%w: invalid file key", ErrManifestStream)
		}
		key, ok := keyValue.(string)
		if !ok || seen[key] {
			return model.ManifestFile{}, fmt.Errorf("%w: invalid or duplicate file key", ErrManifestStream)
		}
		seen[key] = true
		switch key {
		case "path":
			file.Path, err = readManifestString(decoder)
		case "status":
			file.Status, err = readManifestString(decoder)
		case "content_hash":
			var value *string
			value, err = readManifestNullableString(decoder)
			if value != nil {
				hash := model.Hash(*value)
				file.ContentHash = &hash
			}
		case "byte_length":
			var value *int64
			value, err = readManifestNullableInteger(decoder)
			file.ByteLength = value
		case "reason_code":
			var value *string
			value, err = readManifestNullableString(decoder)
			if value != nil {
				identifier := model.Identifier(*value)
				file.ReasonCode = &identifier
			}
		default:
			return model.ManifestFile{}, fmt.Errorf("%w: unknown file field", ErrManifestStream)
		}
		if err != nil {
			return model.ManifestFile{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 5 {
		return model.ManifestFile{}, fmt.Errorf("%w: file object incomplete", ErrManifestStream)
	}
	return file, nil
}

func readManifestHashArray(ctx context.Context, decoder *json.Decoder, parsed *parsedManifestSpools, chunk *[]model.Hash, spoolRoot string, lease ManifestSpoolLease, options CaptureManifestStreamOptions) error {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return fmt.Errorf("%w: dependency digests must be an array", ErrManifestStream)
	}
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parsed.dependencyCount >= options.MaxDependencies {
			return fmt.Errorf("%w: dependency digest limit exceeded", ErrManifestStream)
		}
		hash, err := readManifestHash(decoder)
		if err != nil {
			return err
		}
		*chunk = append(*chunk, hash)
		parsed.dependencyCount++
		if len(*chunk) == manifestRunEntries {
			if err := flushManifestHashRun(ctx, *chunk, parsed, spoolRoot, lease); err != nil {
				return err
			}
			*chunk = make([]model.Hash, 0, manifestRunEntries)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return fmt.Errorf("%w: dependency array is incomplete", ErrManifestStream)
	}
	return nil
}

func readManifestString(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("%w: expected string", ErrManifestStream)
	}
	value, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("%w: expected string", ErrManifestStream)
	}
	return value, nil
}

func readManifestNullableString(decoder *json.Decoder) (*string, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: expected string or null", ErrManifestStream)
	}
	if token == nil {
		return nil, nil
	}
	value, ok := token.(string)
	if !ok {
		return nil, fmt.Errorf("%w: expected string or null", ErrManifestStream)
	}
	return &value, nil
}

func readManifestHash(decoder *json.Decoder) (model.Hash, error) {
	value, err := readManifestString(decoder)
	if err != nil {
		return "", err
	}
	hash, err := model.ParseHash(value)
	if err != nil {
		return "", fmt.Errorf("%w: invalid digest", ErrManifestStream)
	}
	return hash, nil
}

func readManifestInteger(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, fmt.Errorf("%w: expected integer", ErrManifestStream)
	}
	value, ok := token.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%w: expected integer", ErrManifestStream)
	}
	return parseSafeManifestInteger(value)
}

func readManifestNullableInteger(decoder *json.Decoder) (*int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: expected integer or null", ErrManifestStream)
	}
	if token == nil {
		return nil, nil
	}
	value, ok := token.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%w: expected integer or null", ErrManifestStream)
	}
	parsed, err := parseSafeManifestInteger(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func parseSafeManifestInteger(value json.Number) (int64, error) {
	lexeme := string(value)
	if strings.ContainsAny(lexeme, ".eE+") {
		return 0, fmt.Errorf("%w: fractional and exponent numbers are forbidden", ErrManifestStream)
	}
	parsed, err := strconv.ParseInt(lexeme, 10, 64)
	if err != nil || parsed > model.MaxSafeInteger || parsed < -model.MaxSafeInteger {
		return 0, fmt.Errorf("%w: integer outside safe range", ErrManifestStream)
	}
	return parsed, nil
}

func flushManifestFileRun(ctx context.Context, values []model.ManifestFile, parsed *parsedManifestSpools, spoolRoot string, lease ManifestSpoolLease) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) > manifestRunEntries {
		return fmt.Errorf("%w: file sort run exceeds 4096 entries", ErrManifestStream)
	}
	sort.Slice(values, func(i, j int) bool { return bytes.Compare([]byte(values[i].Path), []byte(values[j].Path)) < 0 })
	for index := 1; index < len(values); index++ {
		if values[index-1].Path == values[index].Path {
			return fmt.Errorf("%w: duplicate capture path", ErrManifestStream)
		}
	}
	path := filepath.Join(spoolRoot, fmt.Sprintf("files-run-%06d.bin", len(parsed.fileRuns)))
	file, err := createPrivateSpool(path)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriterSize(&admittedFileWriter{ctx: ctx, lease: lease, file: file}, manifestScratchBuffer)
	for _, value := range values {
		if err := writeManifestFileRecord(buffered, value); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	parsed.fileRuns = append(parsed.fileRuns, path)
	return nil
}

func flushManifestHashRun(ctx context.Context, values []model.Hash, parsed *parsedManifestSpools, spoolRoot string, lease ManifestSpoolLease) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) > manifestRunEntries {
		return fmt.Errorf("%w: dependency sort run exceeds 4096 entries", ErrManifestStream)
	}
	sort.Slice(values, func(i, j int) bool { return bytes.Compare([]byte(values[i]), []byte(values[j])) < 0 })
	path := filepath.Join(spoolRoot, fmt.Sprintf("dependencies-run-%06d.bin", len(parsed.dependencyRuns)))
	file, err := createPrivateSpool(path)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriterSize(&admittedFileWriter{ctx: ctx, lease: lease, file: file}, manifestScratchBuffer)
	for _, value := range values {
		if _, err := io.WriteString(buffered, string(value)); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	parsed.dependencyRuns = append(parsed.dependencyRuns, path)
	return nil
}

func writeManifestFileRecord(writer io.Writer, value model.ManifestFile) error {
	for _, part := range []string{value.Path, value.Status} {
		if err := writeSpoolString(writer, part); err != nil {
			return err
		}
	}
	if value.ContentHash == nil {
		if _, err := writer.Write([]byte{0}); err != nil {
			return err
		}
	} else {
		if _, err := writer.Write([]byte{1}); err != nil {
			return err
		}
		if err := writeSpoolString(writer, string(*value.ContentHash)); err != nil {
			return err
		}
	}
	if value.ByteLength == nil {
		if _, err := writer.Write([]byte{0}); err != nil {
			return err
		}
	} else {
		if _, err := writer.Write([]byte{1}); err != nil {
			return err
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(*value.ByteLength))
		if _, err := writer.Write(encoded[:]); err != nil {
			return err
		}
	}
	if value.ReasonCode == nil {
		_, err := writer.Write([]byte{0})
		return err
	}
	if _, err := writer.Write([]byte{1}); err != nil {
		return err
	}
	return writeSpoolString(writer, string(*value.ReasonCode))
}

func readManifestFileRecord(reader io.Reader) (model.ManifestFile, error) {
	path, err := readSpoolString(reader, 16*1024)
	if err != nil {
		return model.ManifestFile{}, err
	}
	status, err := readSpoolString(reader, 64)
	if err != nil {
		return model.ManifestFile{}, err
	}
	file := model.ManifestFile{Path: path, Status: status}
	flag, err := readSpoolFlag(reader)
	if err != nil {
		return model.ManifestFile{}, err
	}
	if flag == 1 {
		value, err := readSpoolString(reader, 64)
		if err != nil {
			return model.ManifestFile{}, err
		}
		hash := model.Hash(value)
		file.ContentHash = &hash
	}
	flag, err = readSpoolFlag(reader)
	if err != nil {
		return model.ManifestFile{}, err
	}
	if flag == 1 {
		var encoded [8]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return model.ManifestFile{}, err
		}
		value := int64(binary.BigEndian.Uint64(encoded[:]))
		file.ByteLength = &value
	}
	flag, err = readSpoolFlag(reader)
	if err != nil {
		return model.ManifestFile{}, err
	}
	if flag == 1 {
		value, err := readSpoolString(reader, 192)
		if err != nil {
			return model.ManifestFile{}, err
		}
		identifier := model.Identifier(value)
		file.ReasonCode = &identifier
	}
	return file, nil
}

func writeSpoolString(writer io.Writer, value string) error {
	if uint64(len(value)) > math.MaxUint32 {
		return ErrManifestStream
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	if _, err := writer.Write(length[:]); err != nil {
		return err
	}
	_, err := io.WriteString(writer, value)
	return err
}

func readSpoolString(reader io.Reader, maximum uint32) (string, error) {
	var length [4]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return "", err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size > maximum {
		return "", ErrManifestStream
	}
	value := make([]byte, int(size))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}

func readSpoolFlag(reader io.Reader) (byte, error) {
	var flag [1]byte
	if _, err := io.ReadFull(reader, flag[:]); err != nil {
		return 0, err
	}
	if flag[0] > 1 {
		return 0, ErrManifestStream
	}
	return flag[0], nil
}

type fileRunReader struct {
	file *os.File
	path string
	item model.ManifestFile
}

func (reader *fileRunReader) next() error {
	item, err := readManifestFileRecord(reader.file)
	if err == io.EOF {
		return io.EOF
	}
	if err != nil {
		return err
	}
	reader.item = item
	return nil
}

type fileRunHeap []*fileRunReader

func (items fileRunHeap) Len() int { return len(items) }
func (items fileRunHeap) Less(i, j int) bool {
	return bytes.Compare([]byte(items[i].item.Path), []byte(items[j].item.Path)) < 0
}
func (items fileRunHeap) Swap(i, j int)   { items[i], items[j] = items[j], items[i] }
func (items *fileRunHeap) Push(value any) { *items = append(*items, value.(*fileRunReader)) }
func (items *fileRunHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	*items = old[:len(old)-1]
	return last
}

func mergeManifestFileRuns(ctx context.Context, paths []string, outputPath string, lease ManifestSpoolLease) (string, error) {
	if len(paths) > manifestMergeFanIn {
		return "", fmt.Errorf("%w: too many file runs for one bounded merge", ErrManifestStream)
	}
	out, err := createPrivateSpool(outputPath)
	if err != nil {
		return "", err
	}
	buffered := bufio.NewWriterSize(&admittedFileWriter{ctx: ctx, lease: lease, file: out}, manifestScratchBuffer)
	readers := make([]*fileRunReader, 0, len(paths))
	defer func() {
		for _, reader := range readers {
			_ = reader.file.Close()
		}
	}()
	queue := fileRunHeap{}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			_ = out.Close()
			return "", err
		}
		reader := &fileRunReader{file: file, path: path}
		readers = append(readers, reader)
		if err := reader.next(); err == nil {
			heap.Push(&queue, reader)
		} else if err != io.EOF {
			_ = out.Close()
			return "", err
		}
	}
	heap.Init(&queue)
	previous := ""
	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return "", err
		}
		reader := heap.Pop(&queue).(*fileRunReader)
		if previous != "" && reader.item.Path == previous {
			_ = out.Close()
			return "", fmt.Errorf("%w: duplicate capture path across sort runs", ErrManifestStream)
		}
		if err := writeManifestFileRecord(buffered, reader.item); err != nil {
			_ = out.Close()
			return "", err
		}
		previous = reader.item.Path
		if err := reader.next(); err == nil {
			heap.Push(&queue, reader)
		} else if err != io.EOF {
			_ = out.Close()
			return "", err
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return outputPath, nil
}

func mergeManifestHashRuns(ctx context.Context, paths []string, outputPath string, lease ManifestSpoolLease) (string, error) {
	if len(paths) > manifestMergeFanIn {
		return "", fmt.Errorf("%w: too many dependency runs for one bounded merge", ErrManifestStream)
	}
	out, err := createPrivateSpool(outputPath)
	if err != nil {
		return "", err
	}
	buffered := bufio.NewWriterSize(&admittedFileWriter{ctx: ctx, lease: lease, file: out}, manifestScratchBuffer)
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	queue := make(hashRunHeap, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			_ = out.Close()
			return "", err
		}
		files = append(files, file)
		var value [64]byte
		if _, err := io.ReadFull(file, value[:]); err == nil {
			queue = append(queue, hashRunHead{file: file, value: string(value[:])})
		} else if err != io.EOF {
			_ = out.Close()
			return "", err
		}
	}
	heap.Init(&queue)
	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return "", err
		}
		head := heap.Pop(&queue).(hashRunHead)
		if _, err := io.WriteString(buffered, head.value); err != nil {
			_ = out.Close()
			return "", err
		}
		var next [64]byte
		if _, err := io.ReadFull(head.file, next[:]); err == nil {
			head.value = string(next[:])
			heap.Push(&queue, head)
		} else if err != io.EOF {
			_ = out.Close()
			return "", err
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return outputPath, nil
}

type hashRunHead struct {
	file  *os.File
	value string
}
type hashRunHeap []hashRunHead

func (items hashRunHeap) Len() int           { return len(items) }
func (items hashRunHeap) Less(i, j int) bool { return items[i].value < items[j].value }
func (items hashRunHeap) Swap(i, j int)      { items[i], items[j] = items[j], items[i] }
func (items *hashRunHeap) Push(value any)    { *items = append(*items, value.(hashRunHead)) }
func (items *hashRunHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	*items = old[:len(old)-1]
	return last
}

type manifestFileSpoolSource struct {
	path string
	file *os.File
}

func (source *manifestFileSpoolSource) Reset() error {
	if source.file != nil {
		_ = source.file.Close()
	}
	file, err := os.Open(source.path)
	if err != nil {
		return err
	}
	source.file = file
	return nil
}
func (source *manifestFileSpoolSource) Next() (model.ManifestFile, bool, error) {
	if source.file == nil {
		return model.ManifestFile{}, false, errors.New("manifest file spool is not reset")
	}
	value, err := readManifestFileRecord(source.file)
	if err == io.EOF {
		return model.ManifestFile{}, false, nil
	}
	if err != nil {
		return model.ManifestFile{}, false, err
	}
	return value, true, nil
}
func (source *manifestFileSpoolSource) Close() error {
	if source.file == nil {
		return nil
	}
	err := source.file.Close()
	source.file = nil
	return err
}

type manifestHashSpoolSource struct {
	path string
	file *os.File
}

func (source *manifestHashSpoolSource) Reset() error {
	if source.file != nil {
		_ = source.file.Close()
	}
	file, err := os.Open(source.path)
	if err != nil {
		return err
	}
	source.file = file
	return nil
}
func (source *manifestHashSpoolSource) Next() (model.Hash, bool, error) {
	if source.file == nil {
		return "", false, errors.New("manifest dependency spool is not reset")
	}
	var value [64]byte
	_, err := io.ReadFull(source.file, value[:])
	if err == io.EOF {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return model.Hash(value[:]), true, nil
}
func (source *manifestHashSpoolSource) Close() error {
	if source.file == nil {
		return nil
	}
	err := source.file.Close()
	source.file = nil
	return err
}

func spoolFilesEqual(ctx context.Context, leftPath, rightPath string) (bool, error) {
	left, err := os.Open(leftPath)
	if err != nil {
		return false, err
	}
	defer left.Close()
	right, err := os.Open(rightPath)
	if err != nil {
		return false, err
	}
	defer right.Close()
	leftInfo, err := left.Stat()
	if err != nil {
		return false, err
	}
	rightInfo, err := right.Stat()
	if err != nil {
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftBuffer := make([]byte, manifestScratchBuffer)
	rightBuffer := make([]byte, manifestScratchBuffer)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		leftCount, leftErr := io.ReadFull(left, leftBuffer)
		rightCount, rightErr := io.ReadFull(right, rightBuffer)
		if leftCount != rightCount || !bytes.Equal(leftBuffer[:leftCount], rightBuffer[:rightCount]) {
			return false, nil
		}
		if leftErr == io.EOF || leftErr == io.ErrUnexpectedEOF {
			if rightErr != io.EOF && rightErr != io.ErrUnexpectedEOF {
				return false, rightErr
			}
			return true, nil
		}
		if leftErr != nil {
			return false, leftErr
		}
		if rightErr != nil {
			return false, rightErr
		}
	}
}

func validateManifestJSONLexemes(ctx context.Context, file *os.File) error {
	reader := bufio.NewReaderSize(file, manifestScratchBuffer)
	inString := false
	var stringRunes int
	var scanned uint64
	var depth int
	for {
		if scanned&0x3fff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		value, size, err := reader.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if value == utf8.RuneError && size == 1 {
			return fmt.Errorf("%w: manifest is not valid UTF-8", ErrManifestStream)
		}
		scanned++
		if !inString {
			switch value {
			case '{', '[':
				depth++
				if depth > 32 {
					return fmt.Errorf("%w: JSON nesting exceeds 32 levels", ErrManifestStream)
				}
			case '}', ']':
				depth--
				if depth < 0 {
					return fmt.Errorf("%w: JSON nesting is unbalanced", ErrManifestStream)
				}
			case '"':
				inString = true
				stringRunes = 0
			}
			continue
		}
		if value == '"' {
			inString = false
			continue
		}
		if value < 0x20 {
			return fmt.Errorf("%w: unescaped string control character", ErrManifestStream)
		}
		if value == '\\' {
			stringRunes++
			escape, _, err := reader.ReadRune()
			if err != nil {
				return fmt.Errorf("%w: incomplete string escape", ErrManifestStream)
			}
			scanned++
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				unit, err := readJSONUnicodeUnit(reader)
				scanned += 4
				if err != nil {
					return err
				}
				if unit >= 0xd800 && unit <= 0xdbff {
					backslash, _, err := reader.ReadRune()
					if err != nil || backslash != '\\' {
						return fmt.Errorf("%w: unpaired high surrogate", ErrManifestStream)
					}
					u, _, err := reader.ReadRune()
					if err != nil || u != 'u' {
						return fmt.Errorf("%w: unpaired high surrogate", ErrManifestStream)
					}
					low, err := readJSONUnicodeUnit(reader)
					scanned += 6
					if err != nil || low < 0xdc00 || low > 0xdfff {
						return fmt.Errorf("%w: invalid surrogate pair", ErrManifestStream)
					}
				} else if unit >= 0xdc00 && unit <= 0xdfff {
					return fmt.Errorf("%w: unpaired low surrogate", ErrManifestStream)
				}
			default:
				return fmt.Errorf("%w: invalid string escape", ErrManifestStream)
			}
		} else {
			stringRunes++
		}
		if stringRunes > maxManifestStringRunes {
			return fmt.Errorf("%w: string exceeds schema length bound", ErrManifestStream)
		}
	}
	if inString {
		return fmt.Errorf("%w: unterminated JSON string", ErrManifestStream)
	}
	if depth != 0 {
		return fmt.Errorf("%w: JSON nesting is incomplete", ErrManifestStream)
	}
	return nil
}

func readJSONUnicodeUnit(reader *bufio.Reader) (uint16, error) {
	var digits [4]byte
	for index := range digits {
		value, _, err := reader.ReadRune()
		if err != nil || value > 0x7f {
			return 0, fmt.Errorf("%w: malformed Unicode escape", ErrManifestStream)
		}
		digits[index] = byte(value)
	}
	parsed, err := strconv.ParseUint(string(digits[:]), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed Unicode escape", ErrManifestStream)
	}
	return uint16(parsed), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(value)
}
