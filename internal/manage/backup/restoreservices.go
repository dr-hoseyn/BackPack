package backup

import (
	"errors"
	"fmt"
)

// A stopped engine completes its final metrics write before the restored tree
// is installed. If stopping or committing fails, resume every attempted unit
// against the previous configuration.
func commitStoppedRestore(services []string, stop, restart func(string) error, commit func() error) (err error) {
	var attempted []string
	defer func() {
		if err == nil {
			return
		}
		for _, service := range attempted {
			if resumeErr := restart(service); resumeErr != nil {
				err = errors.Join(err, fmt.Errorf("resume %s after failed restore: %w", service, resumeErr))
			}
		}
	}()
	for _, service := range services {
		attempted = append(attempted, service)
		if err := stop(service); err != nil {
			return fmt.Errorf("stop %s before restoring configuration: %w", service, err)
		}
	}
	return commit()
}
