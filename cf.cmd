@echo off
rem ---------------------------------------------------------------------------
rem  CodeForge-Go launcher (cf)
rem
rem  Why this exists:
rem    -config and .env are resolved relative to the CURRENT WORKING DIRECTORY,
rem    not to the executable. Running bin\codeforge.exe from inside bin\ makes the
rem    app silently fall back to built-in defaults (provider=anthropic, no API
rem    key). This launcher always chdir's to the project root first, so it is
rem    safe to call from anywhere.
rem
rem  Usage:
rem    cf                start the service (auto-opens browser)
rem    cf -no-open       start without opening a browser
rem    cf stop           stop the running instance
rem    cf restart        stop then start
rem    cf -workdir D:\x  start with an explicit agent workdir
rem ---------------------------------------------------------------------------
setlocal

cd /d "%~dp0"

rem Append "-config config" unless the caller already passed a -config flag.
set "CFG=-config config"
echo %* | findstr /C:"-config" >nul 2>&1
if not errorlevel 1 set "CFG="

"%~dp0bin\codeforge.exe" %* %CFG%
