<#
.SYNOPSIS
    Rasterize build/appicon.svg into the PNG sizes the Windows icon needs.

.DESCRIPTION
    Step 1 of the icon pipeline documented in AGENTS.md:

        web/src/app/icon.svg   ← the artwork (favicon AND sidebar logo)
        build/appicon.svg      ← the same file, for the desktop pipeline
        [ THIS SCRIPT ]        → build/appicon.png (1024) + icon-N.png
        python build/windows/mkico.py          → build/windows/icon.ico
        go run github.com/tc-hib/go-winres…    → cmd/desktop/rsrc_windows_*.syso

    There is no inkscape/rsvg on a stock Windows box, but Chrome and Edge are
    both Chromium and can screenshot a page headlessly — which is exactly what
    the Wails template's docs mean by "the template's docs use a browser
    screenshot".

    The browser renders ONE large master; every smaller size is a high-quality
    downscale of it (see Resize-Png for why rendering each size directly does
    not work).

    Five traps are handled here, each of which cost real debugging time:

    1. An SVG document loaded directly renders at its INTRINSIC size in the
       corner of the viewport, leaving whitespace. The artwork is therefore
       inlined in an HTML page that stretches it to 100% x 100%.
    2. `--default-background-color=00000000` is required, or the screenshot has
       an opaque WHITE background and the icon becomes a white square.
    3. Chrome/Edge EXIT BEFORE THE BYTES LAND. Waiting on the file length alone
       is not enough: the file reaches its final size while still zero-filled,
       so the header must be validated (and retried) instead.
    4. Piping the browser's stdout (`| Out-Null`) makes it inherit a pipe and
       silently take no screenshot at all. Output goes to a file.
    5. Small `--window-size` values do NOT produce a small render. Windows
       clamps the window to a minimum, so the page is laid out at the larger
       size and the screenshot is a CROPPED, zoomed corner of the artwork —
       a 48px icon came out blank and a 256px icon came out as a detail shot.
       Rendering each size directly is therefore wrong; one 1024 master plus a
       resample is both correct and consistent.

.PARAMETER Browser
    Explicit path to msedge.exe / chrome.exe. Auto-detected when omitted.

.EXAMPLE
    pwsh -NoProfile -File scripts\render-icons.ps1
    make icons
#>
param(
    [string]$Browser = ""
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
$repo = $PWD.Path

function Find-Browser {
    if ($Browser) {
        if (-not (Test-Path $Browser)) { throw "browser not found: $Browser" }
        return $Browser
    }
    $candidates = @(
        "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
        "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
        "$env:LOCALAPPDATA\Google\Chrome\Application\chrome.exe",
        "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe",
        "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"
    )
    foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
    throw "no Chromium browser found; pass -Browser <path>. Neither Chrome nor Edge is installed."
}

# Read-PngHeader parses the IHDR chunk.
#
# PNG integers are BIG-endian, and PowerShell's -shl on a [byte] array element
# yields 0 (it truncates to the element's width before promoting), so a 1024px
# icon "measures" 0x0 and looks like a failed render. Plain arithmetic is the
# form that works.
function Read-PngHeader([string]$Path) {
    $b = [System.IO.File]::ReadAllBytes($Path)
    if ($b.Length -lt 26) { return $null }
    if (-not ($b[0] -eq 0x89 -and $b[1] -eq 0x50 -and $b[2] -eq 0x4e -and $b[3] -eq 0x47)) { return $null }
    [pscustomobject]@{
        Width     = [int]$b[16]*16777216 + [int]$b[17]*65536 + [int]$b[18]*256 + [int]$b[19]
        Height    = [int]$b[20]*16777216 + [int]$b[21]*65536 + [int]$b[22]*256 + [int]$b[23]
        BitDepth  = $b[24]
        ColorType = $b[25]
        Bytes     = $b.Length
    }
}

# Resize-Png writes a high-quality downscale of $Source to $Dest.
#
# Small sizes are RESAMPLED, not rendered: Chrome is driven at one large size
# because a small --window-size gives a cropped, zoomed corner of the artwork
# (Windows clamps the window to a minimum, so the page lays out at the larger
# size and the screenshot captures only the top-left region).
#
# System.Drawing is used rather than adding a dependency. InterpolationMode
# HighQualityBicubic plus PixelOffsetMode.HighQuality is the combination that
# does not fringe the semi-transparent edges of the black outline.
function Resize-Png([string]$Source, [string]$Dest, [int]$Size) {
    Add-Type -AssemblyName System.Drawing
    $src = [System.Drawing.Image]::FromFile($Source)
    try {
        $bmp = New-Object System.Drawing.Bitmap($Size, $Size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
        try {
            $g = [System.Drawing.Graphics]::FromImage($bmp)
            try {
                $g.CompositingMode = [System.Drawing.Drawing2D.CompositingMode]::SourceCopy
                $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
                $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
                $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
                $g.DrawImage($src, (New-Object System.Drawing.Rectangle(0, 0, $Size, $Size)))
            } finally { $g.Dispose() }
            $bmp.Save($Dest, [System.Drawing.Imaging.ImageFormat]::Png)
        } finally { $bmp.Dispose() }
    } finally { $src.Dispose() }
}

$svgPath = Join-Path $repo "build\appicon.svg"
if (-not (Test-Path $svgPath)) { throw "missing build/appicon.svg" }
$svg = Get-Content $svgPath -Raw

$page = @"
<!doctype html><html><head><meta charset="utf-8"><style>
html,body{margin:0;padding:0;width:100%;height:100%;background:transparent;overflow:hidden}
svg{display:block;width:100%;height:100%}
</style></head><body>$svg</body></html>
"@
$htmlPath = Join-Path $repo ".icon-render.html"
[System.IO.File]::WriteAllText($htmlPath, $page, (New-Object System.Text.UTF8Encoding($false)))
$url = "file:///" + $repo.Replace('\', '/') + "/.icon-render.html"

$exe = Find-Browser
Write-Host "==> rasterizing with $(Split-Path $exe -Leaf)" -ForegroundColor Cyan

# appicon.png is the 1024 master (docs/other tooling); icon.ico.png is what
# build/winres.json actually feeds to the Windows resource compiler, and icon-16
# ..48 fill out the sizes the shell picks from.
# Rendered by the browser (the only size it gets right):
$masterPath = Join-Path $repo "build\appicon.png"
$masterSize = 1024
# Derived from the master by resampling:
$resample = @(

    @{ Size = 256;  Out = "build\icon-256.png" },
    @{ Size = 256;  Out = "build\windows\icon.ico.png" },
    @{ Size = 48;   Out = "build\icon-48.png" },
    @{ Size = 32;   Out = "build\icon-32.png" },
    @{ Size = 16;   Out = "build\icon-16.png" }
)
$targets = @(@{ Size = $masterSize; Out = "build\appicon.png" })

$log = Join-Path $repo ".icon-render.log"
$failures = @()

foreach ($t in $targets) {
    $out = Join-Path $repo $t.Out
    if (Test-Path $out) { Remove-Item $out -Force }
    # A FRESH profile per render: a shared one lets the second invocation hand
    # the URL to the already-running browser and exit without screenshotting.
    $profile = Join-Path $repo ".icon-profile-$([guid]::NewGuid().ToString('N').Substring(0,8))"

    $args = @(
        "--headless", "--disable-gpu", "--hide-scrollbars",
        "--no-first-run", "--no-default-browser-check",
        "--user-data-dir=$profile",
        # without this the screenshot has an opaque white background
        "--default-background-color=00000000",
        "--window-size=$($t.Size),$($t.Size)",
        "--screenshot=$out",
        $url
    )
    # Start-Process with FILE handles: piping makes the browser inherit a pipe
    # and take no screenshot.
    Start-Process -FilePath $exe -Wait -WindowStyle Hidden `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" `
        -ArgumentList $args | Out-Null
    Remove-Item -Recurse -Force $profile -ErrorAction SilentlyContinue

    # Validate the HEADER, retrying: the file reaches its final length before
    # the bytes are written, so a length-only wait reads a zero-filled file.
    $info = $null
    for ($i = 0; $i -lt 40; $i++) {
        $info = Read-PngHeader $out
        if ($info -and $info.Width -eq $t.Size -and $info.Height -eq $t.Size) { break }
        Start-Sleep -Milliseconds 250
    }
    if (-not $info) { $failures += "$($t.Out): no PNG produced"; continue }
    if ($info.Width -ne $t.Size -or $info.Height -ne $t.Size) {
        $failures += "$($t.Out): $($info.Width)x$($info.Height), want $($t.Size)"
        continue
    }
    if ($info.ColorType -ne 6) {
        # 6 = RGBA. Anything else lost transparency and would render as a
        # white box on a dark background.
        $failures += "$($t.Out): colorType $($info.ColorType), want 6 (RGBA)"
        continue
    }
    Write-Host ("  {0,-28} {1,5}x{2,-5} RGBA {3,8:N1} KB" -f $t.Out, $info.Width, $info.Height, ($info.Bytes / 1KB))
}

Remove-Item $htmlPath, $log, "$log.err" -Force -ErrorAction SilentlyContinue

if ($failures.Count -eq 0) {
    foreach ($r in $resample) {
        $out = Join-Path $repo $r.Out
        Resize-Png -Source $masterPath -Dest $out -Size $r.Size
        $info = Read-PngHeader $out
        if (-not $info -or $info.Width -ne $r.Size -or $info.ColorType -ne 6) {
            $failures += "$($r.Out): resample produced $($info.Width)x$($info.Height) colorType $($info.ColorType)"
            continue
        }
        Write-Host ("  {0,-28} {1,5}x{2,-5} RGBA {3,8:N1} KB" -f $r.Out, $info.Width, $info.Height, ($info.Bytes / 1KB))
    }
}

if ($failures.Count -gt 0) {
    foreach ($f in $failures) { Write-Host "  FAIL $f" -ForegroundColor Red }
    throw "$($failures.Count) icon render(s) failed"
}
Write-Host "==> ok: PNGs refreshed. Next: make icons (packs the .ico and the .syso)" -ForegroundColor Green
