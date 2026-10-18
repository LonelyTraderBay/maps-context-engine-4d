package sqlite

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const maxAdmissionJournalBytes = 1 << 20

var (
	ErrInvalidAdmissionJournal      = errors.New("invalid volume admission journal")
	ErrAdmissionJournalTooLarge     = errors.New("volume admission journal exceeds size limit")
	ErrAdmissionJournalPath         = errors.New("invalid volume admission journal path")
	ErrAdmissionIdentityMismatch    = errors.New("volume admission journal identity mismatch")
	ErrAdmissionConfigConflict      = errors.New("volume admission config digest conflicts with journal")
	ErrAdmissionReservationOverflow = errors.New("volume admission reservation total exceeds safe integer")
)

type admissionJournalBinding struct {
	VolumeID     model.Identifier
	HomeIdentity model.Identifier
	ConfigDigest model.Hash
}

func encodeAdmissionJournal(journal model.VolumeAdmissionJournal) ([]byte, error) {
	if journal.Entries == nil {
		journal.Entries = []model.VolumeAdmissionEntry{}
	}
	if err := journal.Validate(); err != nil {
		return nil, fmt.Errorf("validate volume admission journal model: %w", err)
	}
	if err := validateAdmissionJournalTimes(journal); err != nil {
		return nil, err
	}
	serialized, err := json.Marshal(journal)
	if err != nil {
		return nil, fmt.Errorf("encode volume admission journal: %w", err)
	}
	value, err := contracts.DecodeJSON(serialized)
	if err != nil {
		return nil, fmt.Errorf("parse encoded volume admission journal: %w", err)
	}
	canonical, err := model.CanonicalJSON1(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize volume admission journal: %w", err)
	}
	if len(canonical) > maxAdmissionJournalBytes {
		return nil, fmt.Errorf("canonical journal has %d bytes: %w", len(canonical), ErrAdmissionJournalTooLarge)
	}
	if err := validatePrivate("VolumeAdmissionJournal", canonical); err != nil {
		return nil, fmt.Errorf("validate canonical volume admission journal: %w", err)
	}
	return canonical, nil
}

type journalReservationTotals struct {
	VolumeRemainingBytes int64
	CacheByWorktree      map[model.UUID]int64
	RenameByWorktree     map[model.UUID]int64
}

func sumAdmissionReservations(journal model.VolumeAdmissionJournal) (journalReservationTotals, error) {
	totals := journalReservationTotals{
		CacheByWorktree:  make(map[model.UUID]int64),
		RenameByWorktree: make(map[model.UUID]int64),
	}
	if _, err := encodeAdmissionJournal(journal); err != nil {
		return journalReservationTotals{}, err
	}
	for _, entry := range journal.Entries {
		volume, ok := addNonnegative(totals.VolumeRemainingBytes, entry.VolumeReservedRemainingBytes)
		if !ok || volume > model.MaxSafeInteger {
			return journalReservationTotals{}, ErrAdmissionReservationOverflow
		}
		totals.VolumeRemainingBytes = volume
		if entry.CacheReservedRemainingBytes == 0 {
			continue
		}
		worktreeID := *entry.WorktreeID
		cache, ok := addNonnegative(totals.CacheByWorktree[worktreeID], entry.CacheReservedRemainingBytes)
		if !ok || cache > model.MaxSafeInteger {
			return journalReservationTotals{}, ErrAdmissionReservationOverflow
		}
		totals.CacheByWorktree[worktreeID] = cache
		if entry.OperationKind == "rename_result" {
			renameBytes, ok := addNonnegative(totals.RenameByWorktree[worktreeID], entry.CacheReservedRemainingBytes)
			if !ok || renameBytes > model.MaxSafeInteger {
				return journalReservationTotals{}, ErrAdmissionReservationOverflow
			}
			totals.RenameByWorktree[worktreeID] = renameBytes
		}
	}
	return totals, nil
}

func validateAdmissionJournalTimes(journal model.VolumeAdmissionJournal) error {
	for _, entry := range journal.Entries {
		createdAt, err := time.Parse(time.RFC3339Nano, entry.CreatedAt)
		if err != nil {
			return fmt.Errorf("invalid admission creation time: %w", ErrInvalidAdmissionJournal)
		}
		_, offset := createdAt.Zone()
		if offset != 0 {
			return fmt.Errorf("admission creation time must be UTC: %w", ErrInvalidAdmissionJournal)
		}
	}
	return nil
}

func decodeAdmissionJournal(raw []byte) (model.VolumeAdmissionJournal, error) {
	var journal model.VolumeAdmissionJournal
	if len(raw) > maxAdmissionJournalBytes {
		return journal, fmt.Errorf("journal has %d bytes: %w", len(raw), ErrAdmissionJournalTooLarge)
	}
	if err := validatePrivate("VolumeAdmissionJournal", raw); err != nil {
		return journal, fmt.Errorf("validate volume admission journal: %w", err)
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("decode volume admission journal: %w", err)
	}
	canonical, err := encodeAdmissionJournal(journal)
	if err != nil {
		return model.VolumeAdmissionJournal{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("journal is not canonical JSON1: %w", ErrInvalidAdmissionJournal)
	}
	return journal, nil
}

func readAdmissionJournal(path string, binding admissionJournalBinding) (model.VolumeAdmissionJournal, error) {
	var journal model.VolumeAdmissionJournal
	cleanPath, err := admissionJournalPath(path)
	if err != nil {
		return journal, err
	}
	pathInfo, err := os.Lstat(cleanPath)
	if err != nil {
		return journal, fmt.Errorf("inspect volume admission journal: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return journal, fmt.Errorf("journal path is not a regular file: %w", ErrAdmissionJournalPath)
	}
	file, err := os.Open(cleanPath)
	if err != nil {
		return journal, fmt.Errorf("open volume admission journal: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return journal, fmt.Errorf("stat opened volume admission journal: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return journal, fmt.Errorf("journal changed while opening: %w", ErrAdmissionJournalPath)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAdmissionJournalBytes+1))
	if err != nil {
		return journal, fmt.Errorf("read volume admission journal: %w", err)
	}
	journal, err = decodeAdmissionJournal(raw)
	if err != nil {
		return journal, err
	}
	if journal.VolumeID != binding.VolumeID || journal.HomeIdentity != binding.HomeIdentity {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("journal volume/home does not match verified binding: %w", ErrAdmissionIdentityMismatch)
	}
	if journal.AdmissionConfigDigest != binding.ConfigDigest {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("journal config digest changed: %w", ErrAdmissionConfigConflict)
	}
	return journal, nil
}

// writeAdmissionJournalAtomic writes in the destination directory so rename
// stays on the same filesystem. T010 must hold the verified volume lease while
// calling this function; this helper is not a cross-process lock.
func writeAdmissionJournalAtomic(path string, journal model.VolumeAdmissionJournal) (retErr error) {
	cleanPath, err := admissionJournalPath(path)
	if err != nil {
		return err
	}
	raw, err := encodeAdmissionJournal(journal)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(cleanPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("journal target is not a regular file: %w", ErrAdmissionJournalPath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect volume admission journal target: %w", err)
	}
	directory := filepath.Dir(cleanPath)
	temporary, err := os.CreateTemp(directory, ".admission-journal-*.tmp")
	if err != nil {
		return fmt.Errorf("create same-volume admission journal temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict admission journal temporary permissions: %w", err)
	}
	written, err := temporary.Write(raw)
	if err == nil && written != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write volume admission journal temporary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("flush volume admission journal temporary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close volume admission journal temporary: %w", err)
	}
	if err := os.Rename(temporaryPath, cleanPath); err != nil {
		return fmt.Errorf("atomically replace volume admission journal: %w", err)
	}
	renamed = true
	return nil
}
