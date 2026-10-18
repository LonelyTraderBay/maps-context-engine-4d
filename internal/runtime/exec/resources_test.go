package exec

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvaluateResourceAdmissionAggregatesSharedVolumeAndRemainingReservations(t *testing.T) {
	now := time.Now()
	destinations := []StorageDestination{
		{Name: "repo-output", VolumeID: "volume-a", PeakRemainingBytes: 50, OtherJobReservations: []StorageReservation{{ID: "job-b", RemainingBytes: 30}}},
		{Name: "cache-output", VolumeID: "volume-a", PeakRemainingBytes: 40, OtherJobReservations: []StorageReservation{{ID: "job-b", RemainingBytes: 30}}},
		{Name: "temp-output", VolumeID: "volume-b", PeakRemainingBytes: 10},
	}
	observations := []ResourceVolumeObservation{
		{VolumeID: "volume-a", AvailableBytes: 140, ObservedAt: now, Scope: "fixture"},
		{VolumeID: "volume-b", AvailableBytes: 10, ObservedAt: now, Scope: "fixture"},
	}
	policy := ResourceAdmissionPolicy{BlockThresholdBytes: 100, WarningThresholdBytes: 150, SafetyReserveBytes: 20, MaxObservationAge: time.Minute}
	decisions, err := EvaluateResourceAdmission(destinations, observations, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].VolumeID != "volume-a" || decisions[0].RequiredBytes != 140 ||
		!decisions[0].Admitted || !decisions[0].Warning || decisions[1].Admitted {
		t.Fatalf("unexpected grouped admission result: %+v", decisions)
	}
}

func TestEvaluateResourceAdmissionRejectsUnknownStaleInvalidAndOverflowInputs(t *testing.T) {
	now := time.Now()
	validDestination := []StorageDestination{{Name: "repo", VolumeID: "a"}}
	validObservation := []ResourceVolumeObservation{{VolumeID: "a", ObservedAt: now, Scope: "fixture"}}
	validPolicy := ResourceAdmissionPolicy{WarningThresholdBytes: 1, MaxObservationAge: time.Minute}
	testCases := []struct {
		name         string
		destinations []StorageDestination
		observations []ResourceVolumeObservation
		policy       ResourceAdmissionPolicy
	}{
		{name: "missing observation", destinations: validDestination},
		{name: "missing scope", destinations: validDestination, observations: []ResourceVolumeObservation{{VolumeID: "a", ObservedAt: now}}},
		{name: "stale observation", destinations: validDestination, observations: []ResourceVolumeObservation{{VolumeID: "a", ObservedAt: now.Add(-time.Hour), Scope: "fixture"}}},
		{name: "future observation", destinations: validDestination, observations: []ResourceVolumeObservation{{VolumeID: "a", ObservedAt: now.Add(time.Second), Scope: "fixture"}}},
		{name: "negative remaining", destinations: []StorageDestination{{Name: "repo", VolumeID: "a", PeakRemainingBytes: -1}}, observations: validObservation},
		{name: "conflicting reservation", destinations: []StorageDestination{{Name: "repo", VolumeID: "a", OtherJobReservations: []StorageReservation{{ID: "job", RemainingBytes: 1}}}, {Name: "cache", VolumeID: "a", OtherJobReservations: []StorageReservation{{ID: "job", RemainingBytes: 2}}}}, observations: validObservation},
		{name: "overflow future demand", destinations: []StorageDestination{{Name: "repo", VolumeID: "a", PeakRemainingBytes: math.MaxInt64}, {Name: "cache", VolumeID: "a", PeakRemainingBytes: 1}}, observations: validObservation},
		{name: "warning below block", destinations: validDestination, observations: validObservation, policy: ResourceAdmissionPolicy{BlockThresholdBytes: 2, WarningThresholdBytes: 1, MaxObservationAge: time.Minute}},
		{name: "missing max age", destinations: validDestination, observations: validObservation, policy: ResourceAdmissionPolicy{WarningThresholdBytes: 1}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.observations != nil && testCase.name != "missing scope" && testCase.name != "stale observation" && testCase.name != "future observation" &&
				testCase.observations[0].Scope == "" {
				testCase.observations[0].Scope = "fixture"
			}
			if testCase.policy == (ResourceAdmissionPolicy{}) {
				testCase.policy = validPolicy
			}
			if _, err := EvaluateResourceAdmission(testCase.destinations, testCase.observations, testCase.policy); err == nil {
				t.Fatal("invalid admission input was accepted")
			}
		})
	}
}

func TestMeasureStorageDestinationsGroupsOpenedVolume(t *testing.T) {
	first := t.TempDir()
	second := filepath.Join(first, "child")
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatal(err)
	}
	destinations, observations, err := measureStorageDestinations([]StorageDestination{
		{Name: "repo", Path: first, PeakRemainingBytes: 1},
		{Name: "cache", Path: second, PeakRemainingBytes: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || destinations[0].VolumeID == "" || destinations[0].VolumeID != destinations[1].VolumeID ||
		observations[0].VolumeID != destinations[0].VolumeID || observations[0].AvailableBytes < 0 || observations[0].ObservedAt.IsZero() || observations[0].Scope == "" {
		t.Fatalf("actual destination measurements were not bound to the shared volume: destinations=%+v observations=%+v", destinations, observations)
	}
}
