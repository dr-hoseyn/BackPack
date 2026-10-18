package cli

import (
	"encoding/json"
	"github.com/backpack/backpack/internal/manage"
	"github.com/backpack/backpack/internal/testhistory"
)

func runTestHistory(args []string) Result {
	asJSON, rest := takeJSONFlag(args)
	if len(rest) != 1 || (rest[0] != "history" && rest[0] != "compare") {
		return fail(CodeUsage, "usage: backpack test history|compare [--json]\n")
	}
	runs, err := manage.LoadConnTestHistory()
	var result Result
	if asJSON {
		var body []byte
		if rest[0] == "compare" {
			body, _ = json.Marshal(testhistory.Compare(runs))
		} else {
			body, _ = json.Marshal(runs)
		}
		result = ok(string(body) + "\n")
	} else {
		result = ok(manage.ConnTestHistoryTable(runs, rest[0] == "compare"))
	}
	if err != nil {
		result.Err = err.Error() + "\n"
		result.Code = CodeFailed
	}
	return result
}
