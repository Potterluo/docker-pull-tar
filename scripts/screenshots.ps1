<#
.SYNOPSIS
  Capture documentation screenshots of the GUI through the Chrome DevTools
  Protocol.

.DESCRIPTION
  Why CDP instead of `chrome --screenshot`:

  - Chrome's one-shot --screenshot flag exits as soon as the load event fires.
    This UI fetches everything AFTER that (search results, task list, artifacts),
    so a plain screenshot catches skeleton placeholders — the same "exits before
    the bytes land" trap that scripts/render-icons.ps1 works around by retrying.
    Over CDP we can wait for the data to actually be there.
  - Some panels only exist after a click (the tag/architecture picker appears once
    a search result is selected). CDP can run that click.
  - The theme lives in localStorage, and it is applied pre-paint, so it has to be
    seeded BEFORE the document loads. Page.addScriptToEvaluateOnNewDocument does
    exactly that.
  - Page.captureScreenshot returns the encoded PNG, so there is no file-length
    race to validate at all.

  It drives one Chrome instance for every shot, with a throwaway profile so a
  stale localStorage cannot leak between captures.

.PARAMETER BaseURL
  The running server, e.g. http://127.0.0.1:8080.

.PARAMETER OutDir
  Where the PNGs go. Default docs/screenshots.

.PARAMETER Theme
  light or dark. Default light.

.PARAMETER Scale
  deviceScaleFactor. 2 gives crisp images on HiDPI displays.

.EXAMPLE
  pwsh -File scripts/screenshots.ps1 -BaseURL http://127.0.0.1:8080
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$BaseURL,
    [string]$OutDir = "docs/screenshots",
    [ValidateSet("light", "dark")][string]$Theme = "light",
    [int]$Width = 1440,
    [int]$Height = 900,
    [double]$Scale = 2,
    [string]$Browser = "",
    # Suffix appended to every file name, so light and dark runs can coexist.
    [string]$Suffix = ""
)

$ErrorActionPreference = "Stop"

function Find-Browser {
    param([string]$Explicit)
    if ($Explicit) {
        if (-not (Test-Path $Explicit)) { throw "browser not found: $Explicit" }
        return $Explicit
    }
    $candidates = @(
        "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
        "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
        "$env:LOCALAPPDATA\Google\Chrome\Application\chrome.exe",
        "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe",
        "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"
    )
    foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
    throw "no Chromium browser found; pass -Browser <path>."
}

function Get-FreePort {
    $l = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $l.Start()
    $port = $l.LocalEndpoint.Port
    $l.Stop()
    return $port
}

# --- a minimal CDP client -----------------------------------------------------

function New-CdpSocket {
    param([string]$WsUrl)
    $ws = [System.Net.WebSockets.ClientWebSocket]::new()
    # Assign the task first: chaining .GetAwaiter().GetResult() straight onto a
    # multi-line method call makes PowerShell resolve the member on the TASK's
    # result type ("VoidTaskResult does not contain a method named SendAsync").
    $task = $ws.ConnectAsync([Uri]$WsUrl, [Threading.CancellationToken]::None)
    # `$null =` is load-bearing: TaskAwaiter.GetResult() on a non-generic Task
    # RETURNS a VoidTaskResult, and PowerShell puts it on the pipeline. Without
    # the discard this function returns [VoidTaskResult, ClientWebSocket] and the
    # caller then asks the first element for .SendAsync — which is exactly the
    # confusing "VoidTaskResult does not contain a method named SendAsync".
    $null = $task.GetAwaiter().GetResult()
    return $ws
}

function Send-Cdp {
    param($Ws, [int]$Id, [string]$Method, $Params = $null)
    $msg = @{ id = $Id; method = $Method }
    if ($null -ne $Params) { $msg.params = $Params }
    $json = $msg | ConvertTo-Json -Depth 20 -Compress
    $bytes = [Text.Encoding]::UTF8.GetBytes($json)
    $seg = [ArraySegment[byte]]::new($bytes)
    $task = $Ws.SendAsync($seg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, [Threading.CancellationToken]::None)
    $null = $task.GetAwaiter().GetResult()
}

function Receive-Cdp {
    param($Ws, [int]$TimeoutMs = 30000)
    $buf = New-Object byte[] 65536
    $ms = [System.IO.MemoryStream]::new()
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ($true) {
        # Poll so a silent tab cannot hang the whole run.
        $task = $Ws.ReceiveAsync([ArraySegment[byte]]::new($buf), [Threading.CancellationToken]::None)
        while (-not $task.IsCompleted) {
            if ((Get-Date) -gt $deadline) { throw "CDP receive timed out" }
            Start-Sleep -Milliseconds 20
        }
        $res = $task.GetAwaiter().GetResult()
        if ($res.MessageType -eq [System.Net.WebSockets.WebSocketMessageType]::Close) { return $null }
        $ms.Write($buf, 0, $res.Count)
        if ($res.EndOfMessage) { break }
    }
    return [Text.Encoding]::UTF8.GetString($ms.ToArray())
}

# Call a CDP method and return its result, skipping event messages.
function Invoke-Cdp {
    param($Ws, [ref]$NextId, [string]$Method, $Params = $null, [int]$TimeoutMs = 30000)
    $id = $NextId.Value
    $NextId.Value = $id + 1
    Send-Cdp -Ws $Ws -Id $id -Method $Method -Params $Params
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ($true) {
        $remaining = [int]($deadline - (Get-Date)).TotalMilliseconds
        if ($remaining -le 0) { throw "CDP call $Method timed out" }
        $raw = Receive-Cdp -Ws $Ws -TimeoutMs $remaining
        if ($null -eq $raw) { throw "CDP socket closed during $Method" }
        $msg = $raw | ConvertFrom-Json
        if ($msg.id -eq $id) {
            if ($msg.error) { throw "CDP $Method failed: $($msg.error.message)" }
            return $msg.result
        }
        # Any other message is an event (loadEventFired, console, …) — ignored.
    }
}

# Poll an expression until it is truthy. Used to wait for React to render the
# data instead of guessing a sleep duration.
function Wait-CdpCondition {
    param($Ws, [ref]$NextId, [string]$Expression, [int]$TimeoutMs = 20000, [string]$What = "condition")
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Cdp -Ws $Ws -NextId $NextId -Method "Runtime.evaluate" -Params @{
            expression = "(() => { try { return !!($Expression) } catch (e) { return false } })()"
            returnByValue = $true
        }
        if ($r.result.value -eq $true) { return $true }
        Start-Sleep -Milliseconds 250
    }
    Write-Warning "timed out waiting for $What; capturing anyway"
    return $false
}

# --- shots --------------------------------------------------------------------
# The 1ms accelerator's row id, so the search screenshot shows a real non-default
# source in the picker rather than whatever happens to be first.
$SourceId = ""
try {
    $srcs = Invoke-RestMethod "$($BaseURL.TrimEnd('/'))/api/sources?kind=search" -TimeoutSec 8
    $one = $srcs.sources | Where-Object { $_.name -like "*1ms*" } | Select-Object -First 1
    if (-not $one) { $one = $srcs.sources | Select-Object -First 1 }
    $SourceId = $one.id
} catch {
    Write-Warning "could not list search sources; the default source will be shown"
}
if ($SourceId) { Write-Host "==> search source: $SourceId" -ForegroundColor DarkGray }

# Each shot is a page plus, optionally, a script to run after it settles and a
# condition that proves the data arrived.
$shots = @(
    @{ Name = "search"
       Path = "/search/?q=nginx&source=$SourceId"
       Wait = "document.body.innerText.includes('共') && document.querySelectorAll('tbody tr').length > 3"
       What = "search results" },
    @{ Name = "download"
       Path = "/search/?q=nginx&source=$SourceId"
       Wait = "document.querySelectorAll('tbody tr').length > 3"
       What = "search results"
       Script = "document.querySelectorAll('tbody tr')[0].click()"
       Then   = "document.body.innerText.includes('选择版本与架构') && document.body.innerText.includes('实际拉取')"
       ThenWhat = "tag/arch panel"
       # Steps 2 and 3 live BELOW the results table, so a plain capture shows
       # only the search list. Scroll the download-source picker to the middle.
       # A whole JS expression, in a single-quoted PowerShell string: embedding a
       # quoted CSS selector inside the double-quoted expression built below
       # produced nested quotes and silently did nothing. The attribute value is
       # unquoted because CSS identifiers admit non-ASCII.
       Scroll = 'document.querySelector("[aria-label=下载源]")?.scrollIntoView({block:"center"})' },
    @{ Name = "tasks"
       Path = "/tasks/"
       Wait = "document.querySelectorAll('tbody tr').length >= 3"
       What = "task rows" },
    @{ Name = "artifacts"
       Path = "/artifacts/"
       Wait = "document.body.innerText.includes('.tar')"
       What = "artifact rows" },
    @{ Name = "credentials"
       Path = "/credentials/"
       Wait = "document.body.innerText.includes('registry.example.com')"
       What = "the stored login" },
    @{ Name = "settings"
       Path = "/settings/"
       Wait = "document.body.innerText.includes('数据源') || document.body.innerText.includes('镜像源')"
       What = "settings cards" }
)

$browserPath = Find-Browser -Explicit $Browser
$port = Get-FreePort
$profile = Join-Path ([System.IO.Path]::GetTempPath()) ("dsh-shots-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $profile | Out-Null
if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Force -Path $OutDir | Out-Null }

Write-Host "==> launching $([IO.Path]::GetFileName($browserPath)) on port $port" -ForegroundColor Cyan
# --headless=new renders like the real browser (old headless dropped some CSS).
# No pipes: a redirected child keeps the pipe open and can hang the caller.
$chromeArgs = @(
    "--headless=new",
    "--remote-debugging-port=$port",
    "--user-data-dir=$profile",
    "--no-first-run", "--no-default-browser-check",
    "--hide-scrollbars",
    "--force-device-scale-factor=$Scale",
    "--window-size=$Width,$Height",
    "about:blank"
)
$proc = Start-Process -FilePath $browserPath -PassThru -ArgumentList $chromeArgs `
    -RedirectStandardOutput (Join-Path $profile "out.log") `
    -RedirectStandardError (Join-Path $profile "err.log")

try {
    # Wait for the debug endpoint. Chrome takes a moment to open the port.
    $version = $null
    for ($i = 0; $i -lt 60; $i++) {
        try { $version = Invoke-RestMethod "http://127.0.0.1:$port/json/version" -TimeoutSec 2; break }
        catch { Start-Sleep -Milliseconds 250 }
    }
    if (-not $version) { throw "Chrome did not expose its debugging port" }
    Write-Host "    $($version.Browser)" -ForegroundColor DarkGray

    $themeScript = @"
try {
  localStorage.setItem('app-theme', '$Theme');
  localStorage.setItem('app-accent', 'violet');
} catch (e) {}
"@

    foreach ($shot in $shots) {
        $url = $BaseURL.TrimEnd("/") + $shot.Path
        Write-Host "==> $($shot.Name)" -ForegroundColor Cyan

        $tab = Invoke-RestMethod "http://127.0.0.1:$port/json/new?about:blank" -Method Put
        $ws = New-CdpSocket -WsUrl $tab.webSocketDebuggerUrl
        try {
            $nextId = [ref]1
            $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Page.enable"
            $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Runtime.enable"
            $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Emulation.setDeviceMetricsOverride" -Params @{
                width = $Width; height = $Height; deviceScaleFactor = $Scale; mobile = $false
            }
            # Seed the theme before ANY document script runs, so the pre-paint
            # snippet in layout.tsx picks it up and there is no flash.
            $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Page.addScriptToEvaluateOnNewDocument" -Params @{
                source = $themeScript
            }

            $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Page.navigate" -Params @{ url = $url }
            # The app fetches after load, so wait for rendered data instead of
            # trusting the load event.
            $null = Wait-CdpCondition -Ws $ws -NextId $nextId -Expression $shot.Wait -What $shot.What

            if ($shot.Script) {
                $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Runtime.evaluate" -Params @{
                    expression = $shot.Script; returnByValue = $true
                }
                if ($shot.Then) {
                    $null = Wait-CdpCondition -Ws $ws -NextId $nextId -Expression $shot.Then -What $shot.ThenWhat
                }
            }

            if ($shot.Scroll) {
                $null = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Runtime.evaluate" -Params @{
                    expression = "$($shot.Scroll); true"
                    returnByValue = $true
                }
                Start-Sleep -Milliseconds 500
            }

            # Let lazy fonts/icons paint before capturing.
            Start-Sleep -Milliseconds 600

            $cap = Invoke-Cdp -Ws $ws -NextId $nextId -Method "Page.captureScreenshot" -Params @{
                format = "png"; captureBeyondViewport = $false
            } -TimeoutMs 60000

            $file = Join-Path $OutDir ("{0}{1}.png" -f $shot.Name, $Suffix)
            [IO.File]::WriteAllBytes($file, [Convert]::FromBase64String($cap.data))

            # Validate the PNG header rather than trusting the write: a 0-byte or
            # HTML error page saved as .png looks fine until someone opens it.
            $bytes = [IO.File]::ReadAllBytes($file)
            if ($bytes.Length -lt 1024) { throw "$($shot.Name): capture is only $($bytes.Length) bytes" }
            if (-not ($bytes[0] -eq 0x89 -and $bytes[1] -eq 0x50 -and $bytes[2] -eq 0x4E -and $bytes[3] -eq 0x47)) {
                throw "$($shot.Name): not a PNG"
            }
            Write-Host ("    {0}  {1}x{2}px  {3:N0} KB" -f (Split-Path $file -Leaf),
                [BitConverter]::ToUInt32($bytes[19..16], 0), [BitConverter]::ToUInt32($bytes[23..20], 0),
                ($bytes.Length / 1KB)) -ForegroundColor DarkGray
        }
        finally {
            try { $ws.Dispose() } catch { }
            try { Invoke-RestMethod "http://127.0.0.1:$port/json/close/$($tab.id)" -TimeoutSec 3 | Out-Null } catch { }
        }
    }
}
finally {
    if ($proc -and -not $proc.HasExited) { $proc | Stop-Process -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 400
    Get-Process -Name "chrome", "msedge" -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $browserPath -and $_.StartTime -gt (Get-Date).AddMinutes(-5) } |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $profile -ErrorAction SilentlyContinue
}

Write-Host "done: $((Get-ChildItem $OutDir -Filter *.png).Count) PNG(s) in $OutDir" -ForegroundColor Green
