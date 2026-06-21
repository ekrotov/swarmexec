package cli

import "testing"

func TestDecideTTY(t *testing.T) {
	tests := []struct {
		name         string
		explicitFlag bool
		flagValue    bool
		stdinIsTerm  bool
		noCommand    bool
		want         bool
	}{
		{"auto: interactive terminal, no command -> tty", false, false, true, true, true},
		{"auto: terminal but explicit command -> no tty", false, false, true, false, false},
		{"auto: piped stdin, no command -> no tty", false, false, false, true, false},
		{"auto: piped stdin with command -> no tty", false, false, false, false, false},
		{"explicit -t forces tty even when piped", true, true, false, false, true},
		{"explicit -t=false forces no tty even on terminal", true, false, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideTTY(tt.explicitFlag, tt.flagValue, tt.stdinIsTerm, tt.noCommand)
			if got != tt.want {
				t.Fatalf("decideTTY(%v,%v,%v,%v) = %v, want %v",
					tt.explicitFlag, tt.flagValue, tt.stdinIsTerm, tt.noCommand, got, tt.want)
			}
		})
	}
}

func TestUptime(t *testing.T) {
	cases := map[string]bool{"-": true} // sanity: zero -> "-"
	if uptime(0) != "-" {
		t.Fatalf("uptime(0) = %q, want -", uptime(0))
	}
	_ = cases
}
