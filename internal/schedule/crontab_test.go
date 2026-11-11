package schedule

import "testing"

// "There is no crontab" and "the crontab could not be read" are not the same
// answer, and treating them as one is how installing a job deletes every other
// job on the machine.
//
// readCrontab returned nil for every failure of `crontab -l`, and SetCron built
// the new crontab from that nil — which is a crontab containing exactly one
// line, ours. So a transient failure turned "schedule an auto-refresh" into
// "replace this machine's crontab with a single backpack entry": the operator's
// backups, their certificate renewals, their own scripts, gone, and the program
// reporting success.
//
// The distinction is made on what cron says. Every implementation in use
// answers a user with no crontab with those words.
func TestAnEmptyCrontabIsToldApartFromOneThatCouldNotBeRead(t *testing.T) {
	empty := []string{
		"no crontab for root\n",
		"no crontab for amin",
		"crontab: no crontab for root",
		"No crontab for root",
	}
	for _, msg := range empty {
		if !noCrontabYet([]byte(msg)) {
			t.Errorf("%q was read as a failure, so scheduling anything on a machine with "+
				"no crontab yet would refuse", msg)
		}
	}

	failures := []string{
		"crontab: installing new crontab\ncrontab: error renaming",
		"/var/spool/cron/crontabs/root: Permission denied",
		"crontab: cannot read /var/spool/cron: Input/output error",
		"",
		"seteuid: Operation not permitted",
	}
	for _, msg := range failures {
		if noCrontabYet([]byte(msg)) {
			t.Errorf("%q was read as an empty crontab — a write built on that replaces "+
				"every job on the machine with ours", msg)
		}
	}
}

func TestCronMarkerMatchesOnlyTheTrailingTagOnAJob(t *testing.T) {
	const marker = "backpack-auto-refresh"
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"0 * * * * backpack --restart-all # backpack-auto-refresh", true},
		{"  0 * * * * backpack --restart-all\t# backpack-auto-refresh  ", true},
		{"0 * * * * other-job # backpack-auto-refresh-disabled", false},
		{"0 * * * * other-job '# backpack-auto-refresh' arg", false},
		{"# 0 * * * * disabled-job # backpack-auto-refresh", false},
		{"NOTE = some-value # backpack-auto-refresh", false},
		{"", false},
	} {
		if got := hasCronMarker(tc.line, marker); got != tc.want {
			t.Errorf("hasCronMarker(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestMarkedIntervalDistinguishesDisabledFromAnUnrecognizedSchedule(t *testing.T) {
	const marker = "backpack-auto-refresh"
	if hours, err := markedIntervalHours(nil, marker); hours != 0 || err != nil {
		t.Fatalf("missing schedule = %d, %v", hours, err)
	}
	if _, err := markedIntervalHours([]string{"@daily backpack --restart-all # " + marker}, marker); err == nil {
		t.Fatal("an unrecognized schedule was treated as disabled")
	}
	if hours, err := markedIntervalHours([]string{"  0 0 */2 * * backpack --restart-all # " + marker}, marker); hours != 48 || err != nil {
		t.Fatalf("48-hour schedule = %d, %v", hours, err)
	}
}
