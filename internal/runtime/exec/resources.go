package exec

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

var ErrResourceAdmission = errors.New("invalid or incomplete resource admission input")

type StorageReservation struct {
	ID             string
	RemainingBytes int64
}

type StorageDestination struct {
	Name                 string
	Path                 string
	VolumeID             string
	PeakRemainingBytes   int64
	OtherJobReservations []StorageReservation
}

type ResourceVolumeObservation struct {
	VolumeID       string
	AvailableBytes int64
	ObservedAt     time.Time
	Scope          string
}

type ResourceAdmissionPolicy struct {
	BlockThresholdBytes   int64
	WarningThresholdBytes int64
	SafetyReserveBytes    int64
	MaxObservationAge     time.Duration
}

type ResourceAdmission struct {
	VolumeID       string
	AvailableBytes int64
	RequiredBytes  int64
	Warning        bool
	Admitted       bool
}

// EvaluateResourceAdmission groups destinations by measured volume, counts a
// reservation identity once per volume, and fails closed on incomplete or stale data.
func EvaluateResourceAdmission(destinations []StorageDestination, observations []ResourceVolumeObservation, policy ResourceAdmissionPolicy) ([]ResourceAdmission, error) {
	if len(destinations) == 0 || len(observations) == 0 || policy.BlockThresholdBytes < 0 ||
		policy.WarningThresholdBytes < policy.BlockThresholdBytes || policy.SafetyReserveBytes < 0 || policy.MaxObservationAge <= 0 {
		return nil, ErrResourceAdmission
	}
	type volumeTotals struct {
		peak         int64
		reservations map[string]int64
	}
	totals := make(map[string]*volumeTotals)
	for _, destination := range destinations {
		if strings.TrimSpace(destination.Name) == "" || strings.TrimSpace(destination.VolumeID) == "" || destination.PeakRemainingBytes < 0 {
			return nil, ErrResourceAdmission
		}
		volume := totals[destination.VolumeID]
		if volume == nil {
			volume = &volumeTotals{reservations: make(map[string]int64)}
			totals[destination.VolumeID] = volume
		}
		var ok bool
		volume.peak, ok = addNonnegativeResource(volume.peak, destination.PeakRemainingBytes)
		if !ok {
			return nil, ErrResourceAdmission
		}
		for _, reservation := range destination.OtherJobReservations {
			if strings.TrimSpace(reservation.ID) == "" || reservation.RemainingBytes < 0 {
				return nil, ErrResourceAdmission
			}
			if prior, exists := volume.reservations[reservation.ID]; exists {
				if prior != reservation.RemainingBytes {
					return nil, ErrResourceAdmission
				}
				continue
			}
			volume.reservations[reservation.ID] = reservation.RemainingBytes
		}
	}

	now := time.Now()
	available := make(map[string]int64, len(observations))
	for _, observation := range observations {
		age := now.Sub(observation.ObservedAt)
		if strings.TrimSpace(observation.VolumeID) == "" || observation.AvailableBytes < 0 || observation.ObservedAt.IsZero() ||
			observation.Scope == "" || age < 0 || age > policy.MaxObservationAge {
			return nil, ErrResourceAdmission
		}
		if _, duplicate := available[observation.VolumeID]; duplicate {
			return nil, ErrResourceAdmission
		}
		available[observation.VolumeID] = observation.AvailableBytes
	}
	if len(available) != len(totals) {
		return nil, ErrResourceAdmission
	}
	volumeIDs := make([]string, 0, len(totals))
	for volumeID := range totals {
		if _, exists := available[volumeID]; !exists {
			return nil, ErrResourceAdmission
		}
		volumeIDs = append(volumeIDs, volumeID)
	}
	sort.Strings(volumeIDs)
	result := make([]ResourceAdmission, 0, len(volumeIDs))
	for _, volumeID := range volumeIDs {
		volume := totals[volumeID]
		reserved := int64(0)
		for _, remaining := range volume.reservations {
			var ok bool
			reserved, ok = addNonnegativeResource(reserved, remaining)
			if !ok {
				return nil, ErrResourceAdmission
			}
		}
		future, ok := addNonnegativeResource(volume.peak, reserved)
		if !ok {
			return nil, ErrResourceAdmission
		}
		required, ok := addNonnegativeResource(future, policy.SafetyReserveBytes)
		if !ok {
			return nil, ErrResourceAdmission
		}
		if required < policy.BlockThresholdBytes {
			required = policy.BlockThresholdBytes
		}
		free := available[volumeID]
		result = append(result, ResourceAdmission{
			VolumeID: volumeID, AvailableBytes: free, RequiredBytes: required,
			Warning: free < policy.WarningThresholdBytes, Admitted: free >= required,
		})
	}
	return result, nil
}

// measureStorageDestinations binds free-space samples to opened directory
// handles. Multiple paths on a volume use the lowest observed headroom.
func measureStorageDestinations(destinations []StorageDestination) ([]StorageDestination, []ResourceVolumeObservation, error) {
	if len(destinations) == 0 {
		return nil, nil, ErrResourceAdmission
	}
	measured := make([]StorageDestination, len(destinations))
	copy(measured, destinations)
	observations := make(map[string]ResourceVolumeObservation)
	for index := range measured {
		destination := &measured[index]
		if destination.Path == "" {
			return nil, nil, ErrResourceAdmission
		}
		volumeID, available, scope, err := measureStoragePath(destination.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("measure declared resource destination: %w", ErrResourceAdmission)
		}
		if destination.VolumeID != "" && destination.VolumeID != volumeID {
			return nil, nil, ErrResourceAdmission
		}
		destination.VolumeID = volumeID
		observation := ResourceVolumeObservation{VolumeID: volumeID, AvailableBytes: available, ObservedAt: time.Now(), Scope: scope}
		if prior, exists := observations[volumeID]; exists {
			if available < prior.AvailableBytes {
				prior.AvailableBytes = available
			}
			if observation.ObservedAt.Before(prior.ObservedAt) {
				prior.ObservedAt = observation.ObservedAt
			}
			if prior.Scope != observation.Scope {
				return nil, nil, ErrResourceAdmission
			}
			observations[volumeID] = prior
		} else {
			observations[volumeID] = observation
		}
	}
	volumeIDs := make([]string, 0, len(observations))
	for volumeID := range observations {
		volumeIDs = append(volumeIDs, volumeID)
	}
	sort.Strings(volumeIDs)
	result := make([]ResourceVolumeObservation, 0, len(volumeIDs))
	for _, volumeID := range volumeIDs {
		result = append(result, observations[volumeID])
	}
	return measured, result, nil
}

func addNonnegativeResource(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}
