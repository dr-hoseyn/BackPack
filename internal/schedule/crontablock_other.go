//go:build !linux

package schedule

import "fmt"

func lockCrontab() (func(), error) {
	return nil, fmt.Errorf("updating system schedules requires Linux file locking")
}
