package cli

import "testing"

func TestDiagnoseCommandIsRoutedAndValidatesArguments(t *testing.T) {
	if !IsCommand("diagnose") {
		t.Fatal("diagnose cannot be dispatched from main")
	}
	for _, args := range [][]string{
		{"diagnose"}, {"diagnose", "-c"}, {"diagnose", "--unknown", "value"},
		{"diagnose", "-c", "one", "-c", "two"},
	} {
		if r := Run(args); r.Code != CodeUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}
