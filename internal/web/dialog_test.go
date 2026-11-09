package web

import (
	"runtime"
	"strings"
	"testing"
)

// TestPowershellPickerArgs pins the Windows picker invocation. The dialogs
// silently fail to appear (or open behind the browser) unless the script runs
// with -STA and each dialog gets a topmost owner window, so guard those here.
// See dialog.go for the rationale.
func TestPowershellPickerArgs(t *testing.T) {
	for _, kind := range []dialogKind{dialogFile, dialogFolder} {
		name, args := powershell(kind)
		if name != "powershell" {
			t.Fatalf("interpreter = %q, want powershell", name)
		}
		joined := strings.Join(args, " ")
		for _, want := range []string{"-STA", "-WindowStyle", "Hidden", "System.Windows.Forms"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%v: args missing %q: %s", kind, want, joined)
			}
		}
		// Both dialogs must pass the owner form to ShowDialog.
		if !strings.Contains(joined, "ShowDialog($o)") {
			t.Errorf("%v: ShowDialog must receive the topmost owner window", kind)
		}
		if !strings.Contains(joined, "TopMost = $true") {
			t.Errorf("%v: owner form must be topmost", kind)
		}
	}
}

// TestUnixPickerMissingHelper reports ErrNoPicker when neither helper exists.
// On non-unix platforms the unix path is not selected, so this is skipped.
func TestUnixPickerMissingHelper(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("unix helper path not used on this platform")
	}
	_, _, ok := unixDialog(dialogFile)
	if !ok {
		t.Log("no zenity/kdialog installed; picker falls back to manual path entry")
	}
}
