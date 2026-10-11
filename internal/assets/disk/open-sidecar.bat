@echo off
rem Windows: double click. Starts the local server and opens the catalog in your browser.
cd /d "%~dp0"
call sidecar.cmd serve
pause
