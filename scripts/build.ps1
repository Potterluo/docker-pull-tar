# Windows build entry point — the PowerShell equivalent of `make build`
# (no make required). Also the authoritative reference for what a full
# build does, on ANY platform:
#
#   1. pnpm install + MARKDOWN_FULL=<full|lite> pnpm build   (static export)
#   2. REPLACE internal/server/dist with web/out             (replace, never
#      merge — a merged copy keeps stale chunks alive in go:embed)
#   3. go build with version/commit/date ldflags
#
# Usage:
#   .\scripts\build.ps1                       # server binary, full markdown
#   .\scripts\build.ps1 -Markdown lite        # lite markdown (-11MB)
#   .\scripts\build.ps1 -Desktop              # also build the Wails shell
param(
    [ValidateSet("full", "lite")]
    [string]$Markdown = "full",
    [switch]$Desktop,
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot\..

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = "dev" }
}
$Commit = (git rev-parse --short HEAD 2>$null)
if (-not $Commit) { $Commit = "unknown" }
$Date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

Write-Host "==> build-web (MARKDOWN_FULL=$Markdown)" -ForegroundColor Cyan
Push-Location web
try {
    pnpm install --frozen-lockfile
    if ($LASTEXITCODE -ne 0) { throw "pnpm install failed" }
    $env:MARKDOWN_FULL = $Markdown
    pnpm build
    if ($LASTEXITCODE -ne 0) { throw "pnpm build failed" }
    Remove-Item Env:MARKDOWN_FULL -ErrorAction SilentlyContinue
} finally {
    Pop-Location
}

Write-Host "==> sync dist (replace, not merge)" -ForegroundColor Cyan
Remove-Item -Recurse -Force internal\server\dist -ErrorAction SilentlyContinue
Copy-Item -Recurse web\out internal\server\dist
# Restore the placeholder the replace above deletes. It is TRACKED, and it is
# what lets a fresh clone — or CI's backend/desktop jobs, which build no
# frontend — compile at all: `go:embed all:dist` fails with "pattern all:dist:
# no matching files found" when the directory does not exist. Forgetting it shows
# up as a deleted file in `git status` and as a red CI on the next push.
$gitkeep = Join-Path "internal\server\dist" ".gitkeep"
if (-not (Test-Path $gitkeep)) { New-Item -ItemType File -Force $gitkeep | Out-Null }

$LdFlags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.date=$Date " +
    "-X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Version=$Version " +
    "-X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Commit=$Commit " +
    "-X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Date=$Date"

Write-Host "==> go build server" -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
go build -ldflags $LdFlags -o bin\dockerpull.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

# Assert-GuiSubsystem reads the PE header's Subsystem field.
#
# A WINDOWS_GUI binary (2) gets no console from Windows; a CONSOLE binary (3)
# opens a command-line window on every launch. The linker flag that decides it
# (-H windowsgui) lives only in this script and the Makefile — nothing in the
# Go source records the intent — so a desktop binary built by hand, or by a
# future script that forgets the flag, silently ships a GUI app that also pops
# up a console. Asserting the ARTIFACT is what makes that impossible.
function Assert-GuiSubsystem([string]$Path) {
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    $peOffset = [BitConverter]::ToInt32($bytes, 0x3C)
    $subsystem = [BitConverter]::ToUInt16($bytes, $peOffset + 0x5C)
    if ($subsystem -ne 2) {
        throw ("$Path has PE subsystem $subsystem (3 = console), so launching it opens a " +
               "command-line window. The desktop build must pass -H windowsgui.")
    }
}

if ($Desktop) {
    Write-Host "==> go build desktop (Wails)" -ForegroundColor Cyan
    $guiFlag = ""
    if ($env:GOOS -eq "windows" -or $null -eq $env:GOOS) { $guiFlag = "-H windowsgui " }
    go build -tags desktop,production -ldflags "$LdFlags $guiFlag" -o bin\dockerpull-desktop.exe ./cmd/desktop
    if ($LASTEXITCODE -ne 0) { throw "desktop build failed" }
    if ($env:GOOS -eq "windows" -or $null -eq $env:GOOS) {
        Assert-GuiSubsystem "bin\dockerpull-desktop.exe"
        Write-Host "    ok: GUI subsystem (no console window)" -ForegroundColor DarkGray
    }
}

Write-Host "==> done: bin\dockerpull.exe$(if ($Desktop) { ' + bin\dockerpull-desktop.exe (run THIS one for the GUI)' })" -ForegroundColor Green
