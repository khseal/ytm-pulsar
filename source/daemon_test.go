package main

import (
	"strings"
	"testing"
)

func TestBootScriptKeepsTheStationDataDir(t *testing.T) {
	script := bootScript("/data/ytm-pulsar")
	for _, want := range []string{
		bootMarker,
		`[ -f "$YAM_ROOT/env.sh" ]`,
		`YTM_ALICE_DATA="${YTM_ALICE_DATA:-$YAM_ROOT/var}"`,
		"daemon start",
		"daemon stop",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "/data/yam") {
		t.Fatal(script)
	}
}
