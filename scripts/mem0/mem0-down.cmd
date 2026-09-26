@echo off
setlocal EnableExtensions
REM ---------------------------------------------------------------------------
REM  Stops the mem0 self-hosted stack. Same file constraints as mem0-up.cmd:
REM  CRLF endings, ASCII only, no command separators inside REM lines, and
REM  capture the code page before chcp. See mem0-up.cmd for the measured
REM  reasons; Chinese prose lives in scripts/mem0/README.md.
REM
REM  Data volumes are kept, so long-term memories survive. Pass --purge to drop
REM  them too, which deletes every memory.
REM ---------------------------------------------------------------------------
for /f "tokens=2 delims=:" %%i in ('chcp') do set "XIMO_OLD_CP=%%i"
chcp 65001 >nul

set "RC=1"
if "%MEM0_DIR%"=="" set "MEM0_DIR=%LOCALAPPDATA%\ximo-agent\third_party\mem0"
set "PURGE=%~1"

where docker >nul 2>nul
if errorlevel 1 (
  echo [FAIL] docker not found, cannot stop the stack.
  echo        Install it: winget install Docker.DockerDesktop
  set "RC=1" & goto :finish
)

if not exist "%MEM0_DIR%\server\docker-compose.yaml" (
  echo [FAIL] no mem0 checkout at %MEM0_DIR%
  echo        Nothing to stop: the stack was never started from this directory.
  set "RC=1" & goto :finish
)

REM Deliberately not an if-block: inside one, the ERRORLEVEL substitution would
REM be expanded while the block is parsed, i.e. before docker runs, so it would
REM read a stale value.
pushd "%MEM0_DIR%\server"
if /I "%PURGE%"=="--purge" goto :purge
echo stopping mem0, keeping the data volume: long-term memory survives
docker compose down
set "RC=%errorlevel%"
goto :stopped
:purge
echo stopping mem0 and DELETING the data volume: every long-term memory goes
docker compose down -v
set "RC=%errorlevel%"
:stopped
popd

if not "%RC%"=="0" (
  echo [FAIL] docker compose down failed with code %RC%.
  echo        Make sure Docker Desktop is running, then rerun this script.
)

:finish
if defined XIMO_OLD_CP chcp%XIMO_OLD_CP% >nul
endlocal & exit /b %RC%
