package manage

import "time"

// Timing and names shared by CLI connection-test runs.

// ConnTestHost is this server's address as a kharej would dial it — the
// default the menu offers — or empty when it is not known.
func ConnTestHost() string { return linkHost() }

// ConnTestJoinWait is how long the Iran side waits for the kharej to check in.
func ConnTestJoinWait() time.Duration { return connTestJoinWait }

// ConnTestRunTime is about how long the tunnels take to be judged once the
// kharej has joined: they are tried side by side, each given its connect wait
// and its one-second echoes, and then the speed runs and the path MTU arrives.
// It is what the panel counts down from, so it errs a little long rather than
// reaching zero with rows still filling.
func ConnTestRunTime() time.Duration {
	return ConnTestRunTimeFor(ConnTestFull)
}

// ConnTestSpeedTime estimates the serial speed phase for healthy tunnels.
func ConnTestSpeedTime(tunnels int) time.Duration {
	return time.Duration(tunnels) * ctSpeedBudget
}

// ConnTestName is a tested transport's name as the menus print it: "TCP MUX",
// "KCP FEC", "Iran→Kharej".
func ConnTestName(tr string) string { return ctName(tr) }
