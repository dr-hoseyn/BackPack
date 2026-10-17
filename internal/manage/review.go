package manage

import (
	"errors"
	"fmt"
	"os"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/tunnelspec"
	"github.com/backpack/backpack/internal/tui"
)

var errEditCancelled = errors.New("edit cancelled; settings unchanged")

// SettingsPreview builds exactly the same candidate as the settings writer.
// Reading the file on both sides of the build also catches external edits.
func SettingsPreview(name string, edit TunnelEdit, direct *DirectEdit) (tunnelspec.EditPreview, error) {
	_, preview, err := settingsCandidate(name, edit, direct)
	return preview, err
}

func settingsCandidate(name string, edit TunnelEdit, direct *DirectEdit) (string, tunnelspec.EditPreview, error) {
	if !validName(name) {
		return "", tunnelspec.EditPreview{}, fmt.Errorf("invalid tunnel name")
	}
	before, err := os.ReadFile(app.ConfigPath(name))
	if err != nil {
		return "", tunnelspec.EditPreview{}, err
	}
	var body string
	if direct != nil {
		body, err = directEditBody(name, *direct)
	} else {
		var spec TunnelSpec
		spec, err = tunnelEditSpec(name, edit)
		if err == nil {
			err = spec.ValidateEdit()
		}
		if err == nil {
			body = spec.Render()
		}
	}
	if err != nil {
		return "", tunnelspec.EditPreview{}, err
	}
	now, err := os.ReadFile(app.ConfigPath(name))
	if err != nil {
		return "", tunnelspec.EditPreview{}, err
	}
	if tunnelspec.ConfigRevision(now) != tunnelspec.ConfigRevision(before) {
		return "", tunnelspec.EditPreview{}, fmt.Errorf("settings changed while preparing the preview; try again")
	}
	preview, err := tunnelspec.PreviewChange(before, []byte(body))
	return body, preview, err
}

func ApplySettingsReviewed(name string, edit TunnelEdit, direct *DirectEdit, revision string) error {
	body, preview, err := settingsCandidate(name, edit, direct)
	if err != nil {
		return err
	}
	if revision == "" || revision != preview.Revision {
		return tunnelspec.ErrSettingsChanged
	}
	return tunnelspec.ApplyBodyReviewed(name, body, "reviewed settings edit", revision)
}

func confirmConfigChanges(preview tunnelspec.EditPreview) bool {
	if len(preview.Changes) == 0 {
		tui.Info("No Settings Changed.")
		return false
	}
	tui.Title("Review Changes")
	tui.Rule()
	for _, change := range preview.Changes {
		tui.Info(change.Field)
		fmt.Println("  " + tui.Color(tui.Gray, change.Before) + " -> " + tui.Color(tui.Bold+tui.White, change.After))
	}
	tui.Rule()
	tui.Warn("The Tunnel Restarts. A Failed Restart Restores The Previous Settings.")
	return tui.Confirm("Apply These Changes", false)
}

func reviewSpec(spec TunnelSpec) error {
	preview, err := tunnelspec.PreviewBody(spec.Name, spec.Render())
	if err != nil {
		return err
	}
	if !confirmConfigChanges(preview) {
		return errEditCancelled
	}
	return tunnelspec.ApplyReviewed(spec, preview.Revision)
}

func reviewBody(name, body string) error {
	preview, err := tunnelspec.PreviewBody(name, body)
	if err != nil {
		return err
	}
	if !confirmConfigChanges(preview) {
		return errEditCancelled
	}
	return tunnelspec.ApplyBodyReviewed(name, body, "terminal edit", preview.Revision)
}

func applySpecUsing(spec TunnelSpec, apply []func(TunnelSpec) error) error {
	if len(apply) > 0 && apply[0] != nil {
		return apply[0](spec)
	}
	return applySpec(spec)
}

func editError(err error) {
	if errors.Is(err, errEditCancelled) {
		tui.Warn(err.Error())
	} else {
		tui.Error("Failed: " + err.Error())
	}
}
