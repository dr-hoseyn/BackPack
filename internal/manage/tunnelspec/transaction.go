package tunnelspec

import (
	"bytes"
	"fmt"
)

// editTransaction is the single commit/rollback sequence used by every edit.
// Its boundaries are explicit so failures can be exercised without systemd.
type editTransaction struct {
	read    func() ([]byte, error)
	write   func([]byte) error
	prepare func() error
	active  func() bool
	restart func() error
	stop    func() error
	ready   func([]byte) bool
	record  func([]byte, string)
}

func (tx editTransaction) apply(body []byte, note, expected string) error {
	prev, err := tx.read()
	if err != nil {
		return fmt.Errorf("could not read the current config: %w", err)
	}
	if expected != "" && ConfigRevision(prev) != expected {
		return ErrSettingsChanged
	}
	preview, err := PreviewChange(prev, body)
	if err != nil {
		return err
	}
	if bytes.Equal(prev, body) || len(preview.Changes) == 0 {
		return nil
	}
	wasActive := tx.active()
	if tx.prepare != nil {
		if err := tx.prepare(); err != nil {
			return err
		}
	}
	if err := tx.write(body); err != nil {
		return fmt.Errorf("could not save settings; current config is unchanged: %w", err)
	}
	failure := tx.restart()
	if failure == nil && !tx.ready(body) {
		failure = fmt.Errorf("the tunnel did not become ready with the new settings")
	}
	if failure != nil {
		if err := tx.write(prev); err != nil {
			return fmt.Errorf("%v; could not restore the previous config: %w", failure, err)
		}
		var rollback error
		if wasActive {
			rollback = tx.restart()
			if rollback == nil && !tx.ready(prev) {
				rollback = fmt.Errorf("the previous service did not become ready")
			}
		} else {
			rollback = tx.stop()
		}
		if rollback != nil {
			return fmt.Errorf("%v; previous config restored but service recovery failed: %w", failure, rollback)
		}
		return fmt.Errorf("%w — reverted to the previous config and service state", failure)
	}
	tx.record(prev, note)
	return nil
}
