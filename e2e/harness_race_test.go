//go:build e2e

package e2e_test

import (
	"reflect"
	"testing"
)

func TestPerformanceRaceReportDetectionCoversEveryChildLifetimeIncludingExpectedCrash(t *testing.T) {
	logs := []string{
		"server started\nserver stopped cleanly\n",
		"server started\nWARNING: DATA RACE\nexit status 66\n",
		"WARNING: DATA RACE\nserver killed by expected SIGKILL (exit status 137)\n",
	}
	if got, want := raceReportProcessIDs(logs), []int{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("race detector process IDs=%v, want %v (normal/crash exit status must not hide report)", got, want)
	}
	if got := raceReportProcessIDs([]string{"server started\n", "server restarted\n"}); len(got) != 0 {
		t.Fatalf("clean child logs reported race detector warnings in processes %v", got)
	}
}

func TestPerformanceE2EServerRaceDetectorConfigurationIsNonSuppressing(t *testing.T) {
	if childRaceDetectorOptions != "halt_on_error=1 exitcode=66" {
		t.Fatalf("race detector options=%q, want halt on report and an unambiguous nonzero code", childRaceDetectorOptions)
	}
}
