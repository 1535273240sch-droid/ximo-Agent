@echo off
setlocal EnableExtensions
REM ---------------------------------------------------------------------------
REM  BEFORE EDITING THIS FILE. Four constraints, all measured on Windows 10
REM  zh-CN with cmd.exe 10.0.19045:
REM   1. CRLF line endings. A LF-only copy makes cmd.exe misparse the file.
REM   2. ASCII ONLY, comments and echoed text both. Non-ASCII bytes make the
REM      parser's byte offset drift: tails of lines get executed as commands and
REM      whole statements get swallowed. It is not loud - it looks like a
REM      working script that silently skipped a step. Chinese prose belongs in
REM      scripts/mem0/README.md, which is not parsed by cmd.
REM   3. Never put a command separator or a redirection operator into a REM line
REM      or into echo text. cmd still honours them there: a separator in a REM
REM      line once re-invoked this script recursively, and two less-than signs
REM      in echo text silently turned whole lines into failed input redirects
REM      ("The system cannot find the file specified") with no output at all.
REM   4. Capture the caller's code page BEFORE chcp. Capturing it afterwards
REM      reads the new value back and the restore becomes a no-op.
REM ---------------------------------------------------------------------------
for /f "tokens=2 delims=:" %%i in ('chcp') do set "XIMO_OLD_CP=%%i"
chcp 65001 >nul

REM ---------------------------------------------------------------------------
REM  Brings up the mem0 self-hosted stack, the backend of XimoAgent long-term
REM  memory. Chinese walkthrough, prerequisites and remedies: see
REM  scripts\mem0\README.md.
REM
REM  Steps: check docker and git; get a mem0 checkout into MEM0_DIR, default
REM  LOCALAPPDATA\ximo-agent\third_party\mem0; generate server\.env plus a
REM  loopback binding override; then run upstream's own docker compose stack
REM  (Postgres+pgvector, API, dashboard) and print the three ports.
REM
REM  Step 3 is the one that used to be missing, and it is why a fresh machine
REM  could never start: upstream server\ ships only .env.example while its
REM  docker-compose.yaml declares env_file: .env and asserts on
REM  POSTGRES_PASSWORD with the required form, so a brand new clone always
REM  failed with "env file not found". The generation itself is implemented
REM  once, in scripts/mem0/setup-stack.sh; this script calls it rather than
REM  keeping a second copy that would drift.
REM
REM  Prerequisite: Docker Desktop - on Windows it needs WSL2 plus
REM  virtualization, and the first install usually requires a reboot.
REM  Usage:  scripts\mem0\mem0-up.cmd [revision]
REM          set MEM0_UP_DRYRUN=1 first to only check prerequisites and
REM          generate .env without cloning or starting anything - use that
REM          when there is no docker or the network is restricted.
REM ---------------------------------------------------------------------------

set "RC=1"
if "%MEM0_DIR%"=="" set "MEM0_DIR=%LOCALAPPDATA%\ximo-agent\third_party\mem0"
set "MEM0_REPO=https://github.com/mem0ai/mem0.git"
REM Pinned to a commit on upstream main instead of tracking the branch: the
REM mem0 REST contract is this project's integration surface, so upstream shape
REM changes should be adopted deliberately, not discovered one morning.
if "%~1"=="" (set "MEM0_REVISION=94c3fe9f238f3dbf29c9ce98643bd71eb13077cd") else (set "MEM0_REVISION=%~1")
set "DRYRUN="
if "%MEM0_UP_DRYRUN%"=="1" set "DRYRUN=1"

REM Locate Git Bash. Generating server\.env lives in setup-stack.sh only - this
REM script does not reimplement it, because two implementations drift apart.
REM Git for Windows ships bash.exe and git is needed anyway to clone. cmd's own
REM "where bash" only finds the usr\bin one, so probe the usual install paths.
set "BASH="
if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not defined BASH if exist "%ProgramFiles(x86)%\Git\bin\bash.exe" set "BASH=%ProgramFiles(x86)%\Git\bin\bash.exe"
if not defined BASH if exist "%LOCALAPPDATA%\Programs\Git\bin\bash.exe" set "BASH=%LOCALAPPDATA%\Programs\Git\bin\bash.exe"
if not defined BASH for /f "delims=" %%i in ('where bash 2^>nul') do if not defined BASH set "BASH=%%i"

echo mem0 checkout dir: %MEM0_DIR%
echo mem0 revision    : %MEM0_REVISION%
if defined DRYRUN echo mode             : DRYRUN - check prerequisites and write .env only
echo.

REM ------------------------------------------------------ [1/4] prerequisites
where docker >nul 2>nul
if errorlevel 1 (
  echo [FAIL] docker not found.
  echo        Install it: winget install Docker.DockerDesktop
  echo        On Windows it needs WSL2 plus virtualization and the first install
  echo        usually needs a reboot. Then confirm with: docker info
  echo        Layer by layer check: bash scripts/mem0/preflight.sh
  set "RC=1" & goto :finish
)

where git >nul 2>nul
if errorlevel 1 (
  echo [FAIL] git not found. It is needed to clone the upstream mem0 checkout.
  echo        Install it: winget install Git.Git
  set "RC=1" & goto :finish
)

REM ---------------------------------------------- [2/4] upstream checkout
set "HAVE_CHECKOUT="
if exist "%MEM0_DIR%\server\docker-compose.yaml" set "HAVE_CHECKOUT=1"

if not defined HAVE_CHECKOUT (
  if defined DRYRUN (
    echo [FAIL] %MEM0_DIR% has no checkout and DRYRUN does not clone.
    echo        Drop MEM0_UP_DRYRUN to let it clone, or place a checkout there
    echo        by hand.
    set "RC=1" & goto :finish
  )
  echo [2/4] cloning mem0 into %MEM0_DIR%
  echo        Upstream is about 65MB. If github.com is unreachable from this
  echo        machine see the codeload fallback in scripts\mem0\README.md.
  if not exist "%MEM0_DIR%" mkdir "%MEM0_DIR%"
  git clone "%MEM0_REPO%" "%MEM0_DIR%"
  if errorlevel 1 (
    echo [FAIL] git clone failed. Check network or proxy. If github.com is
    echo        unreachable, use the codeload tarball documented in
    echo        scripts\mem0\README.md.
    set "RC=1" & goto :finish
  )
)

if exist "%MEM0_DIR%\.git" (
  echo [2/4] checking out %MEM0_REVISION%
  REM A failed fetch is not fatal: the target revision may already be local,
  REM and the checkout below is the real gate.
  git -C "%MEM0_DIR%" fetch --tags --quiet origin
  git -C "%MEM0_DIR%" checkout --quiet "%MEM0_REVISION%"
  if errorlevel 1 (
    echo [FAIL] cannot check out %MEM0_REVISION%. Pass an existing tag or
    echo        commit as the first argument, or get the fetch to succeed.
    set "RC=1" & goto :finish
  )
  git -C "%MEM0_DIR%" rev-parse HEAD > "%MEM0_DIR%\..\mem0-revision.txt"
) else (
  echo [2/4] reusing existing checkout, not a git checkout, skipping pin
)

REM ------------------------------- [3/4] server\.env and loopback override
echo.
echo [3/4] writing server\.env and the loopback binding override

if not defined BASH (
  echo [FAIL] bash not found, and the .env writer needs it. Git for Windows
  echo        ships bash.exe, so install Git first and rerun this script:
  echo          winget install Git.Git
  echo        Manual remedy, in cmd:
  echo          cd /d "%MEM0_DIR%\server"
  echo          copy .env.example .env
  echo        Then edit .env with any editor and put a long random string into
  echo        POSTGRES_PASSWORD and into JWT_SECRET. POSTGRES_PASSWORD is a
  echo        required assertion in the upstream compose file: empty means the
  echo        stack will not start.
  set "RC=1" & goto :finish
)

pushd "%~dp0..\.."
"%BASH%" scripts/mem0/setup-stack.sh
set "ENVRC=%errorlevel%"
popd
if not "%ENVRC%"=="0" (
  echo [FAIL] setup-stack.sh failed. Follow what it printed, then rerun this
  echo        script.
  set "RC=1" & goto :finish
)
if not exist "%MEM0_DIR%\server\.env" (
  echo [FAIL] %MEM0_DIR%\server\.env still does not exist. Starting the stack
  echo        would fail with "env file not found".
  set "RC=1" & goto :finish
)

REM ---------------------------------------------------------- [4/4] stack up
echo.
if defined DRYRUN (
  echo [4/4] DRYRUN: not starting the stack. Prerequisites and .env are fine,
  echo        drop MEM0_UP_DRYRUN to start it.
  set "RC=0" & goto :finish
)

echo [4/4] starting the stack. The first run BUILDS two local images, the mem0
echo        API and the dashboard: upstream compose builds them rather than
echo        pulling ready images, and the dashboard build fetches npm
echo        dependencies. Together with the pgvector image expect roughly ten
echo        to twenty minutes the first time, then the build cache makes it fast.
pushd "%MEM0_DIR%\server"
docker compose config --quiet
if errorlevel 1 (
  echo [FAIL] docker compose config did not pass, so the stack will not start.
  echo        Read its own error above, then match it against this list:
  echo        - env file or POSTGRES_PASSWORD mentioned: regenerate .env with
  echo          BIND=127.0.0.1 bash scripts/mem0/setup-stack.sh
  echo        - yaml tag !override mentioned: Compose is too old, it needs
  echo          v2.24.4 or newer. Upgrade Docker Desktop, or regenerate the
  echo          override with BIND=0.0.0.0 and give up loopback binding.
  popd
  set "RC=1" & goto :finish
)
docker compose up -d --build
set "RC=%errorlevel%"
popd

if not "%RC%"=="0" (
  echo [FAIL] starting the stack failed with code %RC%. Clean up and retry:
  echo          cd /d "%MEM0_DIR%\server"
  echo          docker compose down
  echo        Or read the logs:
  echo          cd /d "%MEM0_DIR%\server"
  echo          docker compose logs --tail=80
  goto :finish
)

echo.
echo [OK] mem0 is up. The three ports, do not mix them up:
echo        REST API   http://127.0.0.1:8888   this is memory.endpoint in config.json
echo        dashboard  http://127.0.0.1:3000   human UI only, it does NOT proxy
echo                                              /memories or /search
echo        postgres   127.0.0.1:8432
echo      All three are bound to loopback only, not reachable from the LAN.
echo.
echo Next steps, in Chinese detail in scripts\mem0\README.md:
echo   1) Open http://127.0.0.1:3000 and finish the setup wizard.
echo   2) On the Configure page set the LLM and the EMBEDDER. The embedder needs
echo      separate credentials: DeepSeek has no embeddings endpoint, and the
echo      server only accepts provider openai or gemini, so Ollama, SiliconFlow
echo      and DashScope all go in as provider=openai plus openai_base_url.
echo      A missing embedder shows up as "writes succeed, recall returns
echo      nothing".
echo   3) Auth: this script writes AUTH_DISABLED=true for local development and
echo      the ports are on loopback. To enable auth set AUTH_DISABLED=false and
echo      MEM0_ADMIN_API_KEY, rerun scripts\mem0\setup-stack.sh, and store the
echo      key in the workbench credential store.
echo   4) Turn on both memory.enabled and feature_flags memory.mem0 in
echo      config.json. Memory only works when both are true.
echo.
echo Four-step integration check:
echo   MEM0_ENDPOINT=http://127.0.0.1:8888 scripts/mem0/verify.sh
echo Docs: scripts\mem0\README.md
set "RC=0"

:finish
if defined XIMO_OLD_CP chcp%XIMO_OLD_CP% >nul
endlocal & exit /b %RC%
