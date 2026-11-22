package syncsched

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.8.25
func TestColdStart_AllowanceDoublesPerRoundToThePoolSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ceiling   int
		successes int
		want      []int
	}{
		{name: "eight workers: 2, 4, 8", ceiling: 8, successes: 14, want: []int{4, 8}},
		{name: "the ceiling bounds the last doubling", ceiling: 6, successes: 6, want: []int{4, 6}},
		{name: "a pool of one admits one and never grows", ceiling: 1, successes: 3, want: nil},
		{name: "a round needs as many successes as the allowance", ceiling: 8, successes: 5, want: []int{4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestScheduler(t, time.Now())
			s.ResetColdStart(tc.ceiling)
			ta := upload(0, "a.txt")
			assert.Equal(t, min(2, tc.ceiling), s.coldStartRampFor(ColdStartTarget(ta)).allowance)

			var raised []int
			for range tc.successes {
				if allowance := s.NoteSuccess(ta); allowance > 0 {
					raised = append(raised, allowance)
				}
			}
			assert.Equal(t, tc.want, raised)
		})
	}
}

// Validates: R-6.8.25
func TestColdStart_HandOverOpensTheTargetToTheScope(t *testing.T) {
	t.Parallel()

	driveA := driveid.New(testDriveID)
	driveB := driveid.New("other-drive")
	s := newTestScheduler(t, time.Now())
	s.ResetColdStart(8)
	a := &syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "a.txt", DriveID: driveA}}
	b := &syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "b.txt", DriveID: driveB}}
	local := &syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "c.txt"}}

	a2 := &syncdomain.TrackedAction{ID: 2, Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "a2.txt", DriveID: driveA}}
	s.Running(a)
	s.Running(a2)
	assert.False(t, s.ColdStartAdmits(&syncdomain.TrackedAction{ID: 3, Action: a.Action}), "two in flight is the opening allowance")
	assert.True(t, s.ColdStartAdmits(b), "each target ramps on its own")
	assert.True(t, s.ColdStartAdmits(local), "an action that sends nothing to a drive is never gated")

	assert.Equal(t, []string{a.Action.ThrottleTargetKey()}, s.HandOverColdStart(syncdomain.SKThrottleDrive(driveA)))
	assert.True(t, s.ColdStartAdmits(&syncdomain.TrackedAction{ID: 3, Action: a.Action}), "the scope owns the drive's admission now")
	assert.Empty(t, s.HandOverColdStart(syncdomain.SKThrottleDrive(driveA)), "said once")
	assert.Empty(t, s.HandOverColdStart(syncdomain.SKQuotaOwn()), "a quota scope says nothing about request rate")
	assert.Zero(t, s.NoteSuccess(a), "a handed-over target no longer ramps")

	assert.ElementsMatch(t, []string{b.Action.ThrottleTargetKey()}, s.HandOverColdStart(syncdomain.SKService()))
	c := &syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "d.txt", DriveID: driveid.New("third-drive")}}
	assert.True(t, s.coldStartRampFor(ColdStartTarget(c)).handedOver, "a service scope takes every later target too")

	s.ResetColdStart(8)
	assert.False(t, s.coldStartRampFor(ColdStartTarget(a)).handedOver, "a fresh plan starts over")
}
