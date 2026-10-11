@echo off
rem Windows: double click. Starts the local server and opens the catalog in your browser.
cd /d "%~dp0"
where python >nul 2>nul
if %errorlevel%==0 (
  python serve.py
  goto :end
)
where py >nul 2>nul
if %errorlevel%==0 (
  py -3 serve.py
  goto :end
)
echo Sidecar needs Python 3.9 or newer, and it was not found.
echo Install it from https://www.python.org/downloads/ and tick "Add python.exe to PATH",
echo then double click this file again.
:end
pause
