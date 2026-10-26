package config

import (
	"reflect"
	"testing"
)

func TestNormalizeMixedFamilyBackups(t *testing.T) {
	got, err := NormalizeFallbackAddrs("192.0.2.1:443", []string{
		" 192.0.2.1 ", "2001:0db8::1", "[2001:db8::1]:443", "[2001:db8::2]", "Backup.Example:8443", "",
	})
	want := []string{"[2001:db8::1]:443", "[2001:db8::2]:443", "backup.example:8443"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("backups = %v, %v; want %v", got, err, want)
	}
	for _, bad := range []string{"[2001:db8::1", "[2001:db8::1]:oops", "host:70000", "host:0", ":443", "host name", "not:an:ipv6"} {
		if _, err := NormalizeFallbackAddrs("192.0.2.1:443", []string{bad}); err == nil {
			t.Errorf("accepted malformed backup %q", bad)
		}
	}
}
