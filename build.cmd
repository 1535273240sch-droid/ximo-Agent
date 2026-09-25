@echo off
REM ============================================================================
REM  XimoAgent v2 build script (Windows / cmd).  ASCII only on purpose:
REM  cmd.exe parses this file in the OEM codepage, so non-ASCII bytes here
REM  would be mangled into bogus commands.
REM
REM  Usage:
REM    build.cmd build        compile all packages
REM    build.cmd test         run all tests
REM    build.cmd test-short   run all tests, skipping the heavy stress test
REM    build.cmd exe          produce dist\ximo-agent.exe
REM    build.cmd package      produce dist\ximo-agent-%VERSION%.zip
REM    build.cmd verify       build + vet + test-short (the acceptance gate)
REM
REM  Env:
REM    GOROOT                 default C:\Users\Administrator\goroot
REM    HTTPS_PROXY/HTTP_PROXY used only if pre-set in the environment
REM ============================================================================

setlocal

if "%GOROOT%"=="" set "GOROOT=C:\Users\Administrator\goroot"
set "PATH=%GOROOT%\bin;%PATH%"

REM Proxy for dependency downloads. Uncomment if your network cannot reach
REM proxy.golang.org directly.
if defined XIMO_USE_PROXY (
  set "HTTPS_PROXY=http://127.0.0.1:7897"
  set "HTTP_PROXY=http://127.0.0.1:7897"
)

set "VERSION=v2.1.0"
set "LDFLAGS=-X main.version=%VERSION%"

set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

if /I "%~1"=="build"      goto :build
if /I "%~1"=="test"       goto :test
if /I "%~1"=="test-short" goto :testshort
if /I "%~1"=="vet"        goto :vet
if /I "%~1"=="fmt"        goto :fmt
if /I "%~1"=="exe"        goto :exe
if /I "%~1"=="package"    goto :package
if /I "%~1"=="verify"     goto :verify
goto :usage

:enter
pushd "%ROOT%\ximo-Agent-2-go" 2>nul || pushd "%ROOT%"
exit /b 0

REM Extra args after the verb are forwarded to `go`, e.g.
REM   build.cmd test ./tests/e2e/
:build
call :enter
go build %2 %3 %4 %5 %6 %7 %8 %9 ./...
set "RC=%errorlevel%"
popd
exit /b %RC%

:test
call :enter
if "%~2"=="" (
  go test ./...
) else (
  go test %2 %3 %4 %5 %6 %7 %8 %9
)
set "RC=%errorlevel%"
popd
exit /b %RC%

:testshort
call :enter
if "%~2"=="" (
  go test -short ./...
) else (
  go test -short %2 %3 %4 %5 %6 %7 %8 %9
)
set "RC=%errorlevel%"
popd
exit /b %RC%

:vet
call :enter
go vet ./...
set "RC=%errorlevel%"
popd
exit /b %RC%

:fmt
call :enter
echo Files needing gofmt ^(pre-existing offenders are expected^):
gofmt -l cmd internal tests release
popd
exit /b 0

:exe
call :enter
if not exist dist mkdir dist
go build -ldflags "%LDFLAGS%" -o dist\ximo-agent.exe .\cmd\ximo-agent\
set "RC=%errorlevel%"
if "%RC%"=="0" echo [OK] dist\ximo-agent.exe
popd
exit /b %RC%

:package
call :enter
call "%~f0" exe
if errorlevel 1 (
  echo [FAIL] could not build the executable
  popd
  exit /b 1
)
if exist dist\ximo-agent-%VERSION%.zip del /q dist\ximo-agent-%VERSION%.zip
REM migrations/ must ship next to the executable: the engine looks for it there.
if exist dist\migrations rmdir /S /Q dist\migrations
xcopy /E /I /Y migrations dist\migrations >nul
copy /Y config.default.json dist\config.default.json >nul
powershell -NoProfile -Command "Compress-Archive -Path 'dist\ximo-agent.exe','dist\migrations','dist\config.default.json' -DestinationPath 'dist\ximo-agent-%VERSION%.zip' -Force"
set "RC=%errorlevel%"
if "%RC%"=="0" echo [OK] dist\ximo-agent-%VERSION%.zip
popd
exit /b %RC%

:verify
echo === build ===
call "%~f0" build
if errorlevel 1 exit /b 1
echo === vet ===
call "%~f0" vet
if errorlevel 1 exit /b 1
echo === test (short) ===
call "%~f0" test-short
if errorlevel 1 exit /b 1
echo === ALL VERIFICATIONS PASSED ===
exit /b 0

:usage
echo Usage: build.cmd {build^|test^|test-short^|vet^|fmt^|exe^|package^|verify}
exit /b 2
