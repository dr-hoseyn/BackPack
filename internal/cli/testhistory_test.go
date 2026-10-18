package cli

import (
	"encoding/json"
	"github.com/backpack/backpack/internal/manage"
	"strings"
	"testing"
)

func TestConnectionTestHistoryCommandsAreRoutableAndReturnEmptyJSONArrays(t *testing.T) {
	old := manage.ConnTestHistoryRoot
	manage.ConnTestHistoryRoot = t.TempDir()
	defer func() { manage.ConnTestHistoryRoot = old }()
	if !IsCommand("test") {
		t.Fatal("main cannot route history commands")
	}
	for _, command := range []string{"history", "compare"} {
		r := Run([]string{"test", command, "--json"})
		var values []any
		if r.Code != CodeOK || json.Unmarshal([]byte(r.Out), &values) != nil || strings.TrimSpace(r.Out) != "[]" {
			t.Fatalf("%s: %+v", command, r)
		}
	}
	for _, args := range [][]string{{"test"}, {"test", "bad"}, {"test", "history", "extra"}} {
		if Run(args).Code != CodeUsage {
			t.Fatal("bad history command accepted")
		}
	}
}
