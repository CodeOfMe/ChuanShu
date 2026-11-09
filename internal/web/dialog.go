package web

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Native file/folder pickers.
//
// A web page cannot read the absolute path of a user-selected file for
// security reasons (the browser hides real paths). Since Chuanshu's UI is a
// local, trusted control plane, we ask the OS for a native picker instead and
// return the chosen absolute path to the page. When no GUI picker is available
// the frontend falls back to manual path entry.

// errCancelled is returned when the user dismisses the dialog without choosing.
var errCancelled = errors.New("cancelled")

// ErrNoPicker indicates the current platform has no supported native picker.
var ErrNoPicker = errors.New("no native file picker available")

// pickerTimeout bounds how long we wait on a modal dialog. Generous, since a
// user may browse for a while, but it prevents a hung helper from pinning a
// request forever.
const pickerTimeout = 5 * time.Minute

// pickFile opens a native "open file" dialog and returns the selected path.
func pickFile() (string, error) { return runPicker(dialogFile) }

// pickFolder opens a native "choose folder" dialog and returns the selected path.
func pickFolder() (string, error) { return runPicker(dialogFolder) }

type dialogKind int

const (
	dialogFile dialogKind = iota
	dialogFolder
)

// runPicker dispatches to a platform-specific dialog implementation.
func runPicker(kind dialogKind) (string, error) {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = appleScript(kind)
	case "windows":
		name, args = powershell(kind)
	default: // linux and other unix-like
		c, a, ok := unixDialog(kind)
		if !ok {
			return "", ErrNoPicker
		}
		name, args = c, a
	}
	return execDialog(name, args)
}

func execDialog(name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pickerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	path := strings.TrimSpace(out.String())
	if err != nil {
		// Distinguish a user cancel from a real failure. Different helpers
		// signal cancel differently; osascript exits non-zero with "-128".
		combined := errb.String()
		if strings.Contains(combined, "-128") ||
			strings.Contains(combined, "User Cancelled") ||
			strings.Contains(combined, "cancelled") {
			return "", errCancelled
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("dialog timed out")
		}
		// A missing helper binary means no picker; others fall back.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// The command never ran (e.g. binary not found).
			return "", ErrNoPicker
		}
		msg := strings.TrimSpace(combined)
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	if path == "" {
		return "", errCancelled
	}
	return path, nil
}

// appleScript builds an osascript argv for the given dialog kind. The chosen
// path is printed as a POSIX path on stdout.
func appleScript(kind dialogKind) (string, []string) {
	var script string
	switch kind {
	case dialogFolder:
		script = `POSIX path of (choose folder with prompt "选择文件夹 / Choose a folder")`
	default:
		script = `POSIX path of (choose file with prompt "选择文件 / Choose a file")`
	}
	return "osascript", []string{"-e", script}
}

// powershell builds a PowerShell argv using Windows Forms file dialogs.
//
// Two details matter for the dialog to actually appear:
//
//   - The script runs with -STA. Windows Forms dialogs require a
//     single-threaded apartment; powershell.exe defaults to STA on modern
//     systems, but a machine-wide registry setting can flip it to MTA, which
//     makes ShowDialog() fail or render nothing.
//   - ShowDialog() gets a real owner window. An ownerless dialog lands at the
//     ownerless z-order, which typically sits *behind* the browser window
//     that issued the request, so it looks like nothing happened. We show a
//     1x1 fully transparent, topmost owner centered on screen: owned dialogs
//     stay above their owner, a topmost owner keeps the dialog above the
//     browser, and the dialog still opens screen-centered because the
//     invisible owner is centered too.
//
// -WindowStyle Hidden keeps powershell's own console window from flashing.
func powershell(kind dialogKind) (string, []string) {
	owner := `$ErrorActionPreference='Stop';` +
		`Add-Type -AssemblyName System.Windows.Forms;` +
		`$o = New-Object System.Windows.Forms.Form;` +
		`$o.Text = ''; $o.ShowInTaskbar = $false;` +
		`$o.TopMost = $true; $o.Opacity = 0;` +
		`$o.Width = 1; $o.Height = 1;` +
		`[void]$o.Show();`
	closeOwner := `; [void]$o.Close()`
	switch kind {
	case dialogFolder:
		script := owner +
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog;` +
			`$d.Description = '选择文件夹 / Choose a folder';` +
			`if ($d.ShowDialog($o) -eq 'OK') { Write-Output $d.SelectedPath }` +
			closeOwner
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-STA", "-WindowStyle", "Hidden", "-Command", script}
	default:
		script := owner +
			`$d = New-Object System.Windows.Forms.OpenFileDialog;` +
			`$d.CheckFileExists = $true;` +
			`$d.Title = '选择文件 / Choose a file';` +
			`if ($d.ShowDialog($o) -eq 'OK') { Write-Output $d.FileName }` +
			closeOwner
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-STA", "-WindowStyle", "Hidden", "-Command", script}
	}
}

// unixDialog finds an available zenity or kdialog and builds its argv.
func unixDialog(kind dialogKind) (string, []string, bool) {
	if p, err := exec.LookPath("zenity"); err == nil {
		args := []string{"--file-selection"}
		if kind == dialogFolder {
			args = append(args, "--directory")
		}
		return p, args, true
	}
	if p, err := exec.LookPath("kdialog"); err == nil {
		if kind == dialogFolder {
			return p, []string{"--getexistingdirectory", "."}, true
		}
		return p, []string{"--getopenfilename"}, true
	}
	return "", nil, false
}
