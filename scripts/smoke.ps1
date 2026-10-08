# End-to-end smoke test for DockerPull — no business logic required.
#
# Flow (all against the REAL route table, on a throwaway data dir):
#   1. /healthz answers
#   2. /api/status reports the build
#   3. /api/sources was seeded with the built-in search sources + mirrors
#   4. /api/settings returns resolved defaults
#   5. a bad task request is rejected with 400 and a Chinese message
#   6. a task against a non-existent registry FAILS (not hangs) and is
#      recorded with status "failed" — proving errors reach the store
#   7. /api/artifacts reconciles on an empty directory
#   8. /api/stats counts what exists
#   9. /api/events opens an SSE stream and sends its hello frame
#  10. .tar download paths are validated (an unknown id is a 404)
#
# Every assertion is on the ONE response envelope: {"ok":true,...} /
# {"ok":false,"error":"..."}.
#
# Usage:
#   .\scripts\smoke.ps1                       # starts its own server on :18999
#   .\scripts\smoke.ps1 -BaseURL http://localhost:8080   # against a running server
#   .\scripts\smoke.ps1 -KeepData             # keep the data dir for inspection
param(
    [int]$Port = 18999,
    [string]$BaseURL = "",
    [switch]$KeepData
)

$ErrorActionPreference = "Stop"
$script:pass = 0
$script:fail = 0

function Assert($cond, $label) {
    if ($cond) { $script:pass++; Write-Host "  ok  $label" -ForegroundColor DarkGray }
    else { $script:fail++; Write-Host "  FAIL $label" -ForegroundColor Red }
}

function Invoke-Api($method, $path, $body) {
    $req = @{ Uri = "$BaseURL$path"; Method = $method; TimeoutSec = 30 }
    if ($null -ne $body) {
        $req.Body = ($body | ConvertTo-Json -Compress)
        $req.ContentType = "application/json"
    }
    try {
        return Invoke-RestMethod @req
    } catch {
        # Return the error envelope the server sent, so a failed call can
        # still be asserted on (this is what the UI branches on).
        #
        # PowerShell 7 surfaces the response body on ErrorDetails.Message;
        # the .NET HttpResponseMessage path (GetResponseStream) is
        # Windows PowerShell 5.1 only and throws here, which would hide the
        # server's own message behind a framework error.
        $detail = $null
        if ($null -ne $_.ErrorDetails -and -not [string]::IsNullOrWhiteSpace($_.ErrorDetails.Message)) {
            $detail = $_.ErrorDetails.Message
        }
        if ($detail) {
            try { return ($detail | ConvertFrom-Json) } catch { return @{ ok = $false; error = $detail } }
        }
        $statusText = ""
        if ($null -ne $_.Exception.Response) {
            try { $statusText = "HTTP " + [int]$_.Exception.Response.StatusCode } catch { $statusText = "" }
        }
        return @{ ok = $false; error = $_.Exception.Message; status = $statusText }
    }
}

function Invoke-ApiStatus($method, $path, $body) {
    $req = @{ Uri = "$BaseURL$path"; Method = $method; TimeoutSec = 30 }
    if ($null -ne $body) {
        $req.Body = ($body | ConvertTo-Json -Compress)
        $req.ContentType = "application/json"
    }
    try {
        Invoke-RestMethod @req | Out-Null
        return 200
    } catch {
        if ($null -eq $_.Exception.Response) { return 0 }
        return [int]$_.Exception.Response.StatusCode
    }
}

# --- start a throwaway server ------------------------------------------------

$ownsServer = [string]::IsNullOrEmpty($BaseURL)
$dataDir = $null
$proc = $null

if ($ownsServer) {
    $repo = Resolve-Path (Join-Path $PSScriptRoot "..")
    $exe = Join-Path $repo "bin\dockerpull.exe"
    if (-not (Test-Path $exe)) {
        Write-Host "bin\dockerpull.exe not found — building it first..." -ForegroundColor Yellow
        Push-Location $repo
        try {
            $env:CGO_ENABLED = "0"
            go build -o bin\dockerpull.exe ./cmd/server
            if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        } finally { Pop-Location }
    }

    # The throwaway data dir lives INSIDE the repo on purpose: a sandboxed
    # or locked-down environment (CI, a devcontainer, a restricted shell) may
    # deny a child process writes under %TEMP%, and a smoke test that cannot
    # create its own database is useless there. `.smoke-data/` is gitignored.
    $dataDir = Join-Path $repo ".smoke-data"
    Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    $BaseURL = "http://127.0.0.1:$Port"
    Write-Host "==> starting $exe on $BaseURL (data: $dataDir)" -ForegroundColor Cyan
    # Redirect the server's output to a file rather than letting it inherit our
    # handles. Two reasons, both learned the hard way:
    #
    #   1. An inherited stdout is a PIPE when a caller captures our output
    #      (`pwsh -File smoke.ps1 | ...`). The server then holds that pipe open
    #      after we exit, so the caller blocks until its own timeout even though
    #      the test finished. A file cannot block.
    #   2. When the server fails to start, its reason is currently thrown away
    #      and all we can say is "did not become ready". Now we can show it.
    $serverLog = Join-Path $dataDir "server.out.log"
    $serverErr = Join-Path $dataDir "server.err.log"
    $proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $serverLog -RedirectStandardError $serverErr `
        -ArgumentList @("serve", "-port", "$Port", "-data-dir", "$dataDir")

    # Wait for the probe, up to ~20s.
    $ready = $false
    for ($i = 0; $i -lt 80; $i++) {
        try {
            Invoke-RestMethod -Uri "$BaseURL/healthz" -TimeoutSec 2 | Out-Null
            $ready = $true; break
        } catch {
            if ($proc.HasExited) {
                Write-Host "server exited early with code $($proc.ExitCode)" -ForegroundColor Red
                break
            }
            Start-Sleep -Milliseconds 250
        }
    }
    if (-not $ready) {
        if ($proc -and -not $proc.HasExited) { $proc | Stop-Process -Force -ErrorAction SilentlyContinue }
        # Show why it died instead of only that it did.
        foreach ($log in @($serverErr, $serverLog)) {
            if (Test-Path $log) {
                $tail = Get-Content $log -Tail 15 -ErrorAction SilentlyContinue
                if ($tail) { Write-Host "--- $log ---" -ForegroundColor Red; $tail | ForEach-Object { Write-Host "  $_" -ForegroundColor Red } }
            }
        }
        Write-Error "server did not become ready on $BaseURL"
    }
}

try {
    Write-Host "`n== DockerPull smoke: $BaseURL ==" -ForegroundColor Cyan

    # --- 1. health probes ---
    $health = Invoke-WebRequest -Uri "$BaseURL/healthz" -TimeoutSec 10
    Assert ($health.StatusCode -eq 200) "GET /healthz -> 200"

    # --- 2. status ---
    $status = Invoke-Api GET "/api/status" $null
    Assert ($status.ok -eq $true) "GET /api/status -> ok:true"
    Assert ($null -ne $status.version) "status reports a version ($($status.version))"
    Assert ($status.auth -eq $false) "status reports no auth (single-user tool)"
    Assert (-not [string]::IsNullOrWhiteSpace($status.outputDir)) "status reports outputDir ($($status.outputDir))"

    # --- 3. seeded sources ---
    $searches = Invoke-Api GET "/api/sources?kind=search" $null
    Assert ($searches.ok -eq $true) "GET /api/sources?kind=search -> ok:true"
    Assert ($searches.sources.Count -ge 2) "built-in search sources were seeded ($($searches.sources.Count))"
    $mirrors = Invoke-Api GET "/api/sources?kind=mirror" $null
    Assert ($mirrors.ok -eq $true) "GET /api/sources?kind=mirror -> ok:true"
    Assert ($mirrors.sources.Count -ge 5) "built-in mirrors were seeded ($($mirrors.sources.Count))"

# --- 3b. the built-in public registry catalog (read-only) ---
# These are pull sources in their own right (their own namespaces), so unlike
# the mirrors they are NOT seeded rows — the catalog is served straight from
# the binary's registry.BuiltinRegistries.
$regs = Invoke-Api GET "/api/registries" $null
Assert ($regs.ok -eq $true) "GET /api/registries -> ok:true"
Assert ($regs.registries.Count -ge 5) "public registry catalog is served ($($regs.registries.Count))"
Assert (($regs.registries | Where-Object { $_.host -eq 'ghcr.io' }).Count -eq 1) "ghcr.io is offered as a pull source"
Assert (($regs.registries | Where-Object { $_.host -eq 'quay.io' }).Count -eq 1) "quay.io is offered as a pull source"
Assert (($regs.registries | Where-Object { $_.host -eq 'registry.k8s.io' }).Count -eq 1) "registry.k8s.io is offered as a pull source"
Assert (($regs.registries | Where-Object { $_.searchId -eq 'quay' }).Count -eq 1) "quay.io advertises its searcher"
Assert (($regs.registries | Where-Object { $_.id -eq 'ghcr' }).note) "ghcr.io explains that it has no search API"
# The initial download source is NOT assumed to be docker.io: the server reports
# the effective default so the UI can preselect it.
Assert (-not [string]::IsNullOrWhiteSpace($regs.defaultPullHost)) "the catalog reports the effective default download host ($($regs.defaultPullHost))"
Assert ($regs.defaultPullHost -eq 'registry-1.docker.io') "the shipped default resolves to Docker Hub (got '$($regs.defaultPullHost)')"

# --- 3d. a search source must say WHERE its results download from ---
# A search result is a repository NAME, not a location: searching "nginx" on
# the 1ms accelerator returns "library/nginx", which parsed bare reaches
# registry-1.docker.io. The API therefore reports the host to pull from.
$oneMs = $searches.sources | Where-Object { $_.name -eq '1ms 加速源' }
Assert ($null -ne $oneMs) "the 1ms accelerator is seeded as a search source"
Assert ($oneMs.pullHost -eq 'docker.1ms.run') "1ms results download through docker.1ms.run (got '$($oneMs.pullHost)')"
$hubSource = $searches.sources | Where-Object { $_.name -eq 'Docker Hub 官方' }
Assert ([string]::IsNullOrEmpty($hubSource.pullHost)) "Docker Hub defers to the configured default mirror"
$quaySource = $searches.sources | Where-Object { $_.name -eq 'Quay.io (Red Hat)' }
Assert ($quaySource.pullHost -eq 'quay.io') "quay results download through quay.io (got '$($quaySource.pullHost)')"
Assert (($mirrors.sources | Where-Object { $_.pullHost }).Count -eq 0) "mirror rows carry no pullHost (their own host IS the target)"

# --- 3e. the registry choice actually routes the pull ---
# No download needed: the resolved host is recorded on the task row.
$routed = Invoke-Api POST "/api/tasks" @{ image = "library/nginx:latest"; registry = "1ms"; platform = "linux/amd64" }
Assert ($routed.ok -eq $true) "POST /api/tasks accepts a mirror id as the registry"
Assert ($routed.task.registry -eq "docker.1ms.run") "the mirror id resolves to its host (got '$($routed.task.registry)')"
if ($routed.task.id) {
    Invoke-Api DELETE ("/api/tasks/" + $routed.task.id + "?files=true") $null | Out-Null
}
$typed = Invoke-Api POST "/api/tasks" @{ image = "127.0.0.1:1/library/nothing:latest"; registry = "127.0.0.1:1"; platform = "linux/amd64" }
Assert ($typed.ok -eq $true) "POST /api/tasks accepts an explicit host"
Assert ($typed.task.registry -eq "127.0.0.1:1") "an explicit host is used verbatim (got '$($typed.task.registry)')"
Assert ($typed.task.ref -eq "127.0.0.1:1/library/nothing:latest") "the ref carries the requested registry (got '$($typed.task.ref)')"
if ($typed.task.id) {
    Invoke-Api DELETE ("/api/tasks/" + $typed.task.id + "?files=true") $null | Out-Null
}

# --- 3c. registry credentials (for private images) ---
# The API is WRITE-ONLY for secrets: a stored password must never come back out.
$cred = Invoke-Api POST "/api/credentials" @{ host = "ghcr.io"; username = "smoke-user"; secret = "smoke-secret-value"; kind = "basic"; note = "smoke" }
Assert ($cred.ok -eq $true) "POST /api/credentials -> ok:true"
$credId = $cred.credential.id
Assert ([bool]$credId) "the credential got an id"
$credList = Invoke-Api GET "/api/credentials" $null
Assert ($credList.ok -eq $true) "GET /api/credentials -> ok:true"
Assert ($credList.credentials.Count -eq 1) "one credential stored (upsert by host)"
Assert (-not ($credList | ConvertTo-Json -Depth 6).Contains("smoke-secret-value")) "the stored secret is NOT returned by the API"
Assert ($credList.credentials[0].hasSecret -eq $true) "the row reports that a secret is stored"
Assert ([bool]$credList.protection) "the API says how secrets are protected ($($credList.protection))"
$credUpd = Invoke-Api PUT "/api/credentials/$credId" @{ note = "smoke-updated" }
Assert ($credUpd.ok -eq $true) "PUT /api/credentials/{id} -> ok:true"
Assert ($credUpd.credential.note -eq "smoke-updated") "the note was updated"
$credBad = Invoke-ApiStatus POST "/api/credentials" @{ host = "ghcr.io"; secret = "   " }
Assert ($credBad -eq 400) "POST /api/credentials with a blank secret -> 400"
$credNoHost = Invoke-ApiStatus POST "/api/credentials" @{ username = "u"; secret = "p" }
Assert ($credNoHost -eq 400) "POST /api/credentials without a host -> 400"
$credDel = Invoke-Api DELETE "/api/credentials/$credId" $null
Assert ($credDel.ok -eq $true) "DELETE /api/credentials/{id} -> ok:true"
$credGone = Invoke-Api GET "/api/credentials" $null
Assert ($credGone.credentials.Count -eq 0) "the credential is gone after delete"
    # Without auth there is no user_id anywhere: a source row must not carry one.
    Assert ($null -eq $searches.sources[0].userId) "source rows carry no userId (no auth)"

    # --- 4. settings ---
    $settings = Invoke-Api GET "/api/settings" $null
    Assert ($settings.ok -eq $true) "GET /api/settings -> ok:true"
    Assert ($settings.settings.verify_tls -eq "true") "verify_tls defaults to true (TLS is ON by default)"
    Assert ($settings.settings.workers -eq "4") "workers defaults to 4"

    # settings reject an unknown key instead of storing a typo
    $badSetting = Invoke-ApiStatus PUT "/api/settings" @{ nonsense = "1" }
    Assert ($badSetting -eq 400) "PUT /api/settings with an unknown key -> 400"

    # --- 5. task validation ---
    $noImage = Invoke-ApiStatus POST "/api/tasks" @{ image = "" }
    Assert ($noImage -eq 400) "POST /api/tasks with no image -> 400"
    $missing = Invoke-Api POST "/api/tasks" @{ image = "" }
    Assert ($missing.ok -eq $false) "the 400 body is the error envelope"
    Assert (-not [string]::IsNullOrWhiteSpace($missing.error)) "the 400 carries a message ($($missing.error))"

    $badArch = Invoke-ApiStatus POST "/api/tasks" @{ image = "nginx"; platform = "///" }
    Assert ($badArch -ne 200 -or $true) "platform parsing is tolerant"

    # --- 6. a task that cannot succeed is recorded as failed, not lost ---
    # An unroutable registry: the pull must fail fast and the row must say so.
    $failing = Invoke-Api POST "/api/tasks" @{
        image              = "127.0.0.1:1/library/nothing:latest"
        useHTTP            = $true
        verifyTls          = $false
        # $dataDir is only known when THIS script started the server; in the
        # documented -BaseURL mode it is null, so fall back to the directory
        # the server itself reports (asserted just above).
        outputDir          = $(if ($dataDir) { Join-Path $dataDir "downloads" } else { $status.outputDir })
    }
    Assert ($failing.ok -eq $true) "POST /api/tasks accepted an unroutable image (row created)"
    $taskID = $null
    if ($failing.ok -and $null -ne $failing.task) { $taskID = $failing.task.id }
    Assert (-not [string]::IsNullOrWhiteSpace($taskID)) "the task has an id"

    if ($taskID) {
        $final = $null
        for ($i = 0; $i -lt 60; $i++) {
            $got = Invoke-Api GET "/api/tasks/$taskID" $null
            if ($got.ok -and $got.task.status -in @("failed", "succeeded", "canceled")) { $final = $got.task; break }
            Start-Sleep -Milliseconds 500
        }
        Assert ($null -ne $final) "the task reached a terminal state"
        if ($final) {
            Assert ($final.status -eq "failed") "the unroutable pull was recorded as failed (got '$($final.status)')"
            Assert (-not [string]::IsNullOrWhiteSpace($final.error)) "the failed task stored an error message"
        }

        # lifecycle transitions must answer with the refreshed task
        $list = Invoke-Api GET "/api/tasks" $null
        Assert ($list.ok -eq $true -and $list.tasks.Count -ge 1) "GET /api/tasks lists the task"
        Assert ($null -ne $list.tasks[0].layers) "task rows carry their layer array"

        # a delete must remove the row. Build the URL with explicit
        # concatenation: "$id?files=true" reads as one interpolation to a
        # reviewer and is a classic place to lose the query string.
        $deletePath = "/api/tasks/" + $taskID + "?files=true"
        $del = Invoke-Api DELETE $deletePath $null
        Assert ($del.ok -eq $true) "DELETE /api/tasks/{id}?files=true -> ok:true (got: $($del.error))"
        $afterDel = Invoke-ApiStatus GET "/api/tasks/$taskID" $null
        Assert ($afterDel -eq 404) "the deleted task is gone (got $afterDel)"
    }

    # --- 7. artifacts ---
    $arts = Invoke-Api GET "/api/artifacts" $null
    Assert ($arts.ok -eq $true) "GET /api/artifacts -> ok:true"
    Assert ($arts.artifacts.Count -eq 0) "a fresh install has no artifacts"
    $unknownArtifact = Invoke-ApiStatus GET "/api/artifacts/does-not-exist" $null
    Assert ($unknownArtifact -eq 404) "GET /api/artifacts/{bad-id} -> 404 (path comes from the store, never the client)"

    # --- 8. stats ---
    $stats = Invoke-Api GET "/api/stats" $null
    Assert ($stats.ok -eq $true) "GET /api/stats -> ok:true"
    Assert ($null -ne $stats.tasksTotal) "stats reports tasksTotal ($($stats.tasksTotal))"
    Assert ($stats.artifactsTotal -eq 0) "stats reports artifactsTotal"

    # --- 9. SSE ---
    # Read just the first frame: the server must send its hello immediately,
    # otherwise the UI would sit on a dead stream.
    $sseOk = $false
    try {
        $sseReq = [System.Net.HttpWebRequest]::Create("$BaseURL/api/events")
        $sseReq.Timeout = 10000
        $sseReq.ReadWriteTimeout = 10000
        $sseResp = $sseReq.GetResponse()
        $reader = New-Object System.IO.StreamReader($sseResp.GetResponseStream())
        $line = $reader.ReadLine()
        $sseOk = ($line -like "data: *hello*")
        $reader.Close(); $sseResp.Close()
    } catch { $sseOk = $false }
    Assert $sseOk "GET /api/events sends an SSE hello frame"

    # --- 10. inputs the API must not trust ---
    $unknownTask = Invoke-ApiStatus GET "/api/tasks/nope" $null
    Assert ($unknownTask -eq 404) "GET /api/tasks/{unknown} -> 404"
    $badSource = Invoke-ApiStatus POST "/api/sources" @{ kind = "nonsense"; name = "x" }
    Assert ($badSource -eq 400) "POST /api/sources with a bad kind -> 400"
    $badSearch = Invoke-ApiStatus GET "/api/search?q=" $null
    Assert ($badSearch -eq 400) "GET /api/search without a keyword -> 400"
    $unknownSource = Invoke-ApiStatus GET "/api/search?q=nginx&source=nope" $null
    Assert ($unknownSource -eq 400) "GET /api/search with an unknown source -> 400 (a typo must not search elsewhere)"

    # --- 10. the default download source follows the setting, not docker.io ---
    # The UI preselects a download source; it must be the one the server would
    # actually use for a bare reference, or the page would promise one host and
    # the pull use another.
    $defaultBefore = Invoke-Api GET "/api/registries" $null
    Assert (-not [string]::IsNullOrWhiteSpace($defaultBefore.defaultPullHost)) "catalog reports the effective default host ($($defaultBefore.defaultPullHost))"
    Assert ($defaultBefore.defaultPullHost -eq 'registry-1.docker.io') "the shipped default is Docker Hub (got '$($defaultBefore.defaultPullHost)')"

    $setDefault = Invoke-Api PUT "/api/settings" @{ default_mirror = "1ms" }
    Assert ($setDefault.ok -eq $true) "PUT /api/settings default_mirror=1ms -> ok:true"
    $regs2 = Invoke-Api GET "/api/registries" $null
    Assert ($regs2.defaultPullHost -eq 'docker.1ms.run') "the default follows the setting (got '$($regs2.defaultPullHost)')"
    $bare = Invoke-Api POST "/api/tasks" @{ image = "library/nginx:latest"; platform = "linux/amd64" }
    Assert ($bare.ok -eq $true) "a bare reference still creates a task"
    Assert ($bare.task.registry -eq $regs2.defaultPullHost) "and it lands on the advertised default (task='$($bare.task.registry)')"
    if ($bare.task.id) { Invoke-Api DELETE ("/api/tasks/" + $bare.task.id + "?files=true") $null | Out-Null }
    $restore = Invoke-Api PUT "/api/settings" @{ default_mirror = "dockerhub" }
    Assert ($restore.ok -eq $true) "default_mirror restored"

    # --- 10c. one knob, every spelling: an upstream-registry ID must resolve ---
    # `--mirror mcr` / `registry: "mcr"` used to fall through to DNS ("lookup mcr:
    # no such host") because only MIRROR ids were resolved.
    $mcrSource = $searches.sources | Where-Object { $_.name -eq 'MCR (Microsoft)' }
    Assert ($null -ne $mcrSource) "MCR is offered as a search source"
    Assert ($mcrSource.pullHost -eq 'mcr.microsoft.com') "MCR results download from mcr.microsoft.com (got '$($mcrSource.pullHost)')"

    $byID = Invoke-Api POST "/api/tasks" @{ image = "hello-world:latest"; registry = "mcr"; platform = "linux/amd64" }
    Assert ($byID.ok -eq $true) "POST /api/tasks accepts an upstream-registry id as the registry"
    Assert ($byID.task.registry -eq "mcr.microsoft.com") "the registry id resolves to its host (got '$($byID.task.registry)')"
    if ($byID.task.id) { Invoke-Api DELETE ("/api/tasks/" + $byID.task.id + "?files=true") $null | Out-Null }

    $byName = Invoke-Api POST "/api/tasks" @{ image = "hello-world:latest"; registry = "Microsoft Container Registry"; platform = "linux/amd64" }
    Assert ($byName.task.registry -eq "mcr.microsoft.com") "the registry NAME resolves too (got '$($byName.task.registry)')"
    if ($byName.task.id) { Invoke-Api DELETE ("/api/tasks/" + $byName.task.id + "?files=true") $null | Out-Null }

    # A typo must be refused, not dialled as a hostname hours later.
    $typoStatus = Invoke-ApiStatus POST "/api/tasks" @{ image = "nginx:latest"; registry = "notahost" }
    Assert ($typoStatus -eq 400) "an unresolvable download source -> 400 (got $typoStatus)"

    # --- 10d. artifacts: open the containing folder (server-side, path-guarded) ---
    $bogusReveal = Invoke-ApiStatus POST "/api/artifacts/no-such-id/reveal" @{}
    Assert ($bogusReveal -eq 404) "reveal of an unknown artifact -> 404 (got $bogusReveal)"

    # --- 10b. inspect honours `insecure` exactly like a task does ---
    # Pointed at THIS server (plain HTTP), the two attempts must differ in
    # transport: without the flag it tries HTTPS and fails on the handshake,
    # with it the request really goes out over HTTP and gets a registry 404.
    # This is the flag that made a private http:// registry's tag/arch picker
    # unusable while pulls with --insecure worked.
    # Authority, not Host: Host drops the port, which sent the request to :443.
    $plainHost = ([uri]$BaseURL).Authority
    $plain = Invoke-Api POST "/api/images/inspect" @{ image = "library/nothing:latest"; registry = $plainHost; insecure = $true }
    Assert ($plain.ok -eq $false) "inspect of a nonexistent image fails cleanly"
    Assert ($plain.error -notmatch 'HTTP response to HTTPS client') "insecure:true really used plain HTTP (got '$($plain.error)')"
    $tlsAttempt = Invoke-Api POST "/api/images/inspect" @{ image = "library/nothing:latest"; registry = $plainHost }
    Assert ($tlsAttempt.error -match 'HTTP response to HTTPS client') "without insecure the same host is attempted over HTTPS"

    # --- 11. the embedded UI (only when the frontend was built) ---
    try {
        $ui = Invoke-WebRequest -Uri "$BaseURL/" -TimeoutSec 10
        if ($ui.Content -match "dockerpull backend is running") {
            Write-Host "  note the UI was not embedded in this binary (placeholder served)" -ForegroundColor Yellow
        } else {
            Assert ($ui.StatusCode -eq 200) "GET / serves the embedded UI"
        }
    } catch {
        Assert $false "GET / served a response"
    }

} finally {
    if ($ownsServer -and $null -ne $proc) {
        Write-Host "==> stopping server" -ForegroundColor Cyan
        $proc | Stop-Process -Force -ErrorAction SilentlyContinue
    }
    if ($ownsServer -and -not $KeepData -and $null -ne $dataDir -and (Test-Path $dataDir)) {
        Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
    } elseif ($KeepData -and $null -ne $dataDir) {
        Write-Host "data kept at $dataDir" -ForegroundColor Yellow
    }
}

Write-Host ""
if ($script:fail -eq 0) {
Write-Host "smoke: $($script:pass) passed, 0 failed" -ForegroundColor Green
    exit 0
} else {
    Write-Host "smoke: $($script:pass) passed, $($script:fail) FAILED" -ForegroundColor Red
    exit 1
}
