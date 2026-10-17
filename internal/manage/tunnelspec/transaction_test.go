package tunnelspec

import (
	"errors"
	"strings"
	"testing"
)

func TestEditTransactionFailuresRestoreState(t *testing.T) {
	before := []byte("[l3]\nmtu=1400\ntoken='keep-secret'\n")
	after := []byte("[l3]\nmtu=1300\ntoken='keep-secret'\n")
	for _, tc := range []struct {
		name                                                   string
		active, failRestart, failReady, failWrite, failRestore bool
		expected                                               string
	}{
		{name: "successful edit", active: true},
		{name: "failed restart", active: true, failRestart: true, expected: "reverted"},
		{name: "failed readiness", active: true, failReady: true, expected: "reverted"},
		{name: "previously stopped", failRestart: true, expected: "reverted"},
		{name: "failed write", active: true, failWrite: true, expected: "unchanged"},
		{name: "failed recovery is explicit", active: true, failRestart: true, failRestore: true, expected: "recovery failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append([]byte{}, before...)
			restarts, stops, recorded, writes := 0, 0, 0, 0
			tx := editTransaction{
				read: func() ([]byte, error) { return body, nil },
				write: func(next []byte) error {
					writes++
					if tc.failWrite {
						return errors.New("disk full")
					}
					body = append([]byte{}, next...)
					return nil
				},
				active: func() bool { return tc.active },
				restart: func() error {
					restarts++
					if tc.failRestart && (restarts == 1 || tc.failRestore) {
						return errors.New("restart refused")
					}
					return nil
				},
				stop:  func() error { stops++; return nil },
				ready: func(next []byte) bool { return !(tc.failReady && string(next) == string(after)) },
				record: func(prev []byte, note string) {
					recorded++
					if string(prev) != string(before) || note != "test edit" {
						t.Error("incorrect history entry")
					}
				},
			}
			err := tx.apply(after, "test edit", ConfigRevision(before))
			if tc.expected == "" {
				if err != nil || string(body) != string(after) || recorded != 1 || restarts != 1 {
					t.Fatalf("successful commit: err=%v record=%d restart=%d", err, recorded, restarts)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.expected) || string(body) != string(before) || recorded != 0 {
					t.Fatalf("rollback: err=%v body=%s history=%d", err, body, recorded)
				}
				if !tc.active && stops != 1 {
					t.Error("previously stopped service was not stopped again")
				}
				if tc.failWrite && (restarts != 0 || writes != 1) {
					t.Error("a failed atomic write restarted the service")
				}
			}
		})
	}
}

func TestEditTransactionRejectsStaleInvalidAndUnchanged(t *testing.T) {
	before := []byte("[server]\nports=['443']\n")
	for _, tc := range []struct {
		name     string
		body     []byte
		revision string
		fail     bool
	}{
		{"stale preview", []byte("[server]\nports=['8080']\n"), "outdated", true},
		{"invalid candidate", []byte("[server"), "", true},
		{"unchanged", before, "", false},
		{"formatting only", []byte("[server]\nports = [ '443' ]\n"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := editTransaction{read: func() ([]byte, error) { return before, nil }, active: func() bool { t.Fatal("no-op or refused edit queried the service"); return false }}
			err := tx.apply(tc.body, "", tc.revision)
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v, want failure=%v", err, tc.fail)
			}
		})
	}
}

func TestEditPreviewRedactsCredentialsAndSortsChanges(t *testing.T) {
	before := []byte("[server]\nports=['443']\ntoken='old-token'\nproxy='socks5://user:secret@proxy:1080'\n")
	after := []byte("[server]\nports=['443=8443']\ntoken='new-token'\nproxy='socks5://user:new-secret@proxy:1080'\n")
	p, err := PreviewChange(before, after)
	if err != nil || len(p.Changes) != 3 || p.Revision != ConfigRevision(before) {
		t.Fatalf("preview=%+v err=%v", p, err)
	}
	for i, c := range p.Changes {
		if i > 0 && p.Changes[i-1].Field > c.Field {
			t.Error("changes are not ordered")
		}
		if c.Field != "server.ports" && (c.Before != "Hidden value" || c.After != "Hidden value") {
			t.Errorf("credential exposed: %+v", c)
		}
	}
}

func TestEditTransactionReportsAnUnrecoverableConfigWrite(t *testing.T) {
	before := []byte("[l3]\nmtu=1400\n")
	after := []byte("[l3]\nmtu=1300\n")
	writes := 0
	tx := editTransaction{
		read: func() ([]byte, error) { return before, nil },
		write: func([]byte) error {
			writes++
			if writes == 2 {
				return errors.New("disk unavailable")
			}
			return nil
		},
		active:  func() bool { return true },
		restart: func() error { return errors.New("restart refused") },
	}
	err := tx.apply(after, "", "")
	if err == nil || !strings.Contains(err.Error(), "could not restore the previous config") || strings.Contains(err.Error(), "reverted to") {
		t.Fatalf("rollback failure misreported: %v", err)
	}
}

func TestEditTransactionStopsBeforeWritingWhenPreparationFails(t *testing.T) {
	before := []byte("[l3]\nmtu=1400\n")
	after := []byte("[l3]\nmtu=1300\n")
	failure := errors.New("configuration storage unavailable")
	for _, stage := range []string{"read", "prepare"} {
		t.Run(stage, func(t *testing.T) {
			prepared := false
			tx := editTransaction{
				read: func() ([]byte, error) {
					if stage == "read" {
						return nil, failure
					}
					return before, nil
				},
				active: func() bool { return true },
				prepare: func() error {
					prepared = true
					return failure
				},
				write: func([]byte) error {
					t.Fatal("a failed prerequisite wrote the config")
					return nil
				},
				restart: func() error {
					t.Fatal("a failed prerequisite restarted the service")
					return nil
				},
			}
			if err := tx.apply(after, "", ""); !errors.Is(err, failure) {
				t.Fatalf("prerequisite error was lost: %v", err)
			}
			if prepared != (stage == "prepare") {
				t.Fatalf("prepare called after %s failure: %v", stage, prepared)
			}
		})
	}
}
