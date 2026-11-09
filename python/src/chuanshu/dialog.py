"""Native file/folder pickers (macOS, Windows, Linux).

A browser page cannot read the absolute path of a user-selected file, so the
local UI asks the OS for a native dialog and receives the chosen path. When no
GUI picker is available the frontend falls back to manual path entry.
"""
from __future__ import annotations

import shutil
import subprocess
import sys


class NoPickerError(Exception):
    pass


class CancelledError(Exception):
    pass


def _run(argv: list, timeout: int = 300) -> str:
    try:
        proc = subprocess.run(
            argv, capture_output=True, text=True, timeout=timeout)
    except FileNotFoundError as e:
        raise NoPickerError(f"no native picker available: {e}") from e
    except subprocess.TimeoutExpired as e:
        raise RuntimeError("dialog timed out") from e
    err = (proc.stderr or "").strip()
    if proc.returncode != 0:
        # osascript signals user cancel with "-128" in stderr.
        if "-128" in err or "cancel" in err.lower():
            raise CancelledError()
        raise RuntimeError(err or f"picker exited {proc.returncode}")
    path = (proc.stdout or "").strip()
    if not path:
        raise CancelledError()
    return path


# Windows: -STA forces a single-threaded apartment (WinForms dialogs require
# it), -WindowStyle Hidden stops the PowerShell console flashing. The dialog
# gets a real owner: an ownerless ShowDialog() lands at the ownerless z-order,
# usually *behind* the browser, so the user thinks nothing happened. The owner
# is a 1x1 transparent topmost form at screen centre -- invisible, but it pulls
# the owned dialog above the browser and keeps the dialog screen-centred.
# Same design as Go internal/web/dialog.go.
_PS_WIN_OWNER = (
    "$ErrorActionPreference='Stop';"
    "Add-Type -AssemblyName System.Windows.Forms;"
    "$o = New-Object System.Windows.Forms.Form;"
    "$o.Text = ''; $o.ShowInTaskbar = $false;"
    "$o.TopMost = $true; $o.Opacity = 0;"
    "$o.Width = 1; $o.Height = 1;"
    "[void]$o.Show();"
)
_PS_WIN_CLOSE = "; [void]$o.Close()"


def _win_argv(script: str) -> list:
    return ["powershell", "-NoProfile", "-NonInteractive", "-STA",
            "-WindowStyle", "Hidden", "-Command", script]


def pick_file() -> str:
    if sys.platform == "darwin":
        script = 'POSIX path of (choose file with prompt "选择文件 / Choose a file")'
        return _run(["osascript", "-e", script])
    if sys.platform.startswith("win"):
        ps = (
            _PS_WIN_OWNER +
            "$d = New-Object System.Windows.Forms.OpenFileDialog;"
            "$d.CheckFileExists = $true;"
            "$d.Title = '选择文件 / Choose a file';"
            "if ($d.ShowDialog($o) -eq 'OK') { Write-Output $d.FileName }"
            + _PS_WIN_CLOSE
        )
        return _run(_win_argv(ps))
    if shutil.which("zenity"):
        return _run(["zenity", "--file-selection"])
    if shutil.which("kdialog"):
        return _run(["kdialog", "--getopenfilename"])
    raise NoPickerError("no native file picker (install zenity or kdialog)")


def pick_folder() -> str:
    if sys.platform == "darwin":
        script = 'POSIX path of (choose folder with prompt "选择文件夹 / Choose a folder")'
        return _run(["osascript", "-e", script])
    if sys.platform.startswith("win"):
        ps = (
            _PS_WIN_OWNER +
            "$d = New-Object System.Windows.Forms.FolderBrowserDialog;"
            "$d.Description = '选择文件夹 / Choose a folder';"
            "if ($d.ShowDialog($o) -eq 'OK') { Write-Output $d.SelectedPath }"
            + _PS_WIN_CLOSE
        )
        return _run(_win_argv(ps))
    if shutil.which("zenity"):
        return _run(["zenity", "--file-selection", "--directory"])
    if shutil.which("kdialog"):
        return _run(["kdialog", "--getexistingdirectory", "."])
    raise NoPickerError("no native folder picker (install zenity or kdialog)")
