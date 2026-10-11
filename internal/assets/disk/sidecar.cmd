@echo off
rem Runs the Sidecar program built for this computer.
set "arch=amd64"
if /I "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "arch=arm64"
set "bin=%~dp0bin\sidecar-windows-%arch%.exe"
if not exist "%bin%" (
  echo This disk has no Sidecar program for windows/%arch%. Available:
  dir /b "%~dp0bin"
  echo Download the build for this computer from the Disk Sidecar releases and run:  sidecar install ^<this disk^>
  exit /b 1
)
"%bin%" %*
