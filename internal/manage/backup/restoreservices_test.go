package backup

import (
	"errors"
	"slices"
	"testing"
)

func TestRestoreCommitsAfterEveryEngineHasStopped(t *testing.T) {
	var calls []string
	err := commitStoppedRestore([]string{"a", "b"}, func(name string) error {
		calls = append(calls, "stop "+name)
		return nil
	}, func(name string) error {
		calls = append(calls, "resume "+name)
		return nil
	}, func() error {
		calls = append(calls, "commit")
		return nil
	})
	want := []string{"stop a", "stop b", "commit"}
	if err != nil || !slices.Equal(calls, want) {
		t.Fatalf("restore calls = %v, error %v, want %v", calls, err, want)
	}
}

func TestRestoreResumesEnginesAndReportsRecoveryFailures(t *testing.T) {
	stopFailure := errors.New("stop failed")
	commitFailure := errors.New("commit failed")
	resumeFailure := errors.New("resume failed")
	for _, failStop := range []bool{false, true} {
		var calls []string
		err := commitStoppedRestore([]string{"a", "b", "c"}, func(name string) error {
			calls = append(calls, "stop "+name)
			if failStop && name == "b" {
				return stopFailure
			}
			return nil
		}, func(name string) error {
			calls = append(calls, "resume "+name)
			if name == "a" {
				return resumeFailure
			}
			return nil
		}, func() error {
			calls = append(calls, "commit")
			return commitFailure
		})
		want := []string{"stop a", "stop b", "stop c", "commit", "resume a", "resume b", "resume c"}
		failure := commitFailure
		if failStop {
			want = []string{"stop a", "stop b", "resume a", "resume b"}
			failure = stopFailure
		}
		if !errors.Is(err, failure) || !errors.Is(err, resumeFailure) || !slices.Equal(calls, want) {
			t.Fatalf("stop failure=%t: calls=%v, error=%v, want %v", failStop, calls, err, want)
		}
	}
}
