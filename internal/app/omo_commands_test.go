package app

import "testing"

func TestIsOMOAgentMatchesOnlyOmoPanes(t *testing.T) {
	for agent, want := range map[string]bool{
		"omo": true, " OMO ": true, "omo-cli": true,
		"omp": false, "pi": false, "claude": false, "": false, "omnibus": false,
	} {
		if got := isOMOAgent(agent); got != want {
			t.Errorf("isOMOAgent(%q) = %v, want %v", agent, got, want)
		}
	}
}

func TestIsOMOBinaryRejectsOtherAgentExecutables(t *testing.T) {
	for path, want := range map[string]bool{
		"/home/user/.local/bin/omo": true,
		"/usr/bin/omo-ai":           true,
		"/usr/local/bin/omp":        false,
		"/usr/local/bin/pi":         false,
		"/opt/omnibus":              false,
		"":                          false,
	} {
		if got := isOMOBinary(path); got != want {
			t.Errorf("isOMOBinary(%q) = %v, want %v", path, got, want)
		}
	}
}
