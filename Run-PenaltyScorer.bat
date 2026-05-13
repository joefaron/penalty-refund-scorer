@echo off
REM Penalty Refund scorer - folder picker launcher.
REM Double-click this file. Pick the folder that holds your IRS Account
REM Transcript PDFs (subfolders OK; recursive). The CSV lands inside that
REM folder as penalty-refund-findings.csv.

setlocal
set "SCRIPT_DIR=%~dp0"

REM Pop a Windows folder picker via PowerShell. PS writes the selection to
REM stdout; we capture it into FOLDER. Cancel -> empty FOLDER -> exit clean.
for /f "usebackq delims=" %%F in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "Add-Type -AssemblyName System.Windows.Forms | Out-Null; $d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description = 'Pick the folder that contains your IRS Account Transcript PDFs (subfolders OK)'; $d.ShowNewFolderButton = $false; if ($d.ShowDialog() -eq 'OK') { Write-Output $d.SelectedPath }"`) do set "FOLDER=%%F"

if not defined FOLDER (
  echo No folder picked. Exiting.
  pause
  exit /b 1
)

echo.
echo Scoring transcripts under:
echo   %FOLDER%
echo.

"%SCRIPT_DIR%go-scorer.exe" --root "%FOLDER%"
set EXITCODE=%ERRORLEVEL%

echo.
if "%EXITCODE%"=="0" (
  echo Done. Two files in the folder above:
  echo   penalty-refund-findings.csv               - one row per finding (open in Excel)
  echo   penalty-refund-findings-ingest-summary.txt - what was parsed vs skipped, and why
  echo.
  REM Auto-open the summary so the user sees what got ingested. Notepad is
  REM blocking, so the pause prompt fires only after they close it.
  if exist "%FOLDER%\penalty-refund-findings-ingest-summary.txt" (
    start "" notepad "%FOLDER%\penalty-refund-findings-ingest-summary.txt"
  )
) else (
  echo Scorer exited with code %EXITCODE%.
)

pause
endlocal
