# btop hub agent: uninstall on Windows.
#
# Paste into an elevated Windows PowerShell (Start, type "powershell", choose "Run as
# administrator"):
#
#   & ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('__HUB_URL__/uninstall.ps1')))
#
# It stops and deletes the btop-agent service, then removes "%ProgramFiles%\btop-agent" (the
# program) and "%ProgramData%\btop-agent" (the config holding the token, and the log). Running it
# again, or on a machine without the agent, is harmless.
#
# Keep this file plain ASCII and compatible with Windows PowerShell 5.1. It runs as a script
# block, where 'exit' would close the user's window, so failures throw instead.

$ErrorActionPreference = 'Stop'

$ServiceName = 'btop-agent'
$ProcessName = 'btop-agent'
$ScExe = Join-Path -Path (Join-Path -Path $env:SystemRoot -ChildPath 'System32') -ChildPath 'sc.exe'

# Runs a console program and throws unless its exit code is one of $OkExitCodes. Windows
# PowerShell 5.1 turns stderr lines into error records, which 'Stop' would make fatal, so the
# preference is relaxed inside this function and only the exit code counts.
function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [int[]]$OkExitCodes = @(0)
    )
    $ErrorActionPreference = 'Continue'
    $global:LASTEXITCODE = -1
    try {
        $output = @(& $FilePath @ArgumentList 2>&1 | ForEach-Object { [string]$_ })
    } catch {
        throw ('Could not run {0}: {1}' -f $FilePath, $_.Exception.Message)
    }
    $code = $LASTEXITCODE
    if ($OkExitCodes -notcontains $code) {
        $text = ($output | Where-Object { $_.Trim() -ne '' }) -join ' '
        throw ('{0} {1} failed with exit code {2}: {3}' -f (Split-Path -Path $FilePath -Leaf), ($ArgumentList -join ' '), $code, $text)
    }
}

# Waits up to $Seconds for the service to reach $Status: 'Stopped' or 'Gone' (no longer
# registered). Returns $true if it did.
function Wait-AgentService {
    param([string]$Status, [int]$Seconds)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ($true) {
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($Status -eq 'Gone') {
            if (-not $service) { return $true }
        } elseif (-not $service) {
            if ($Status -eq 'Stopped') { return $true }
        } elseif ([string]$service.Status -eq $Status) {
            return $true
        }
        if ((Get-Date) -ge $deadline) { return $false }
        Start-Sleep -Milliseconds 500
    }
}

# Stops the service, waiting up to 20 s. A stop sent while it is still starting is refused, so
# it is sent again until it takes. If the agent hangs, its process is ended.
function Stop-AgentService {
    $deadline = (Get-Date).AddSeconds(20)
    while ($true) {
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if (-not $service) { return }
        $status = [string]$service.Status
        if ($status -eq 'Stopped') { return }
        if ((Get-Date) -ge $deadline) { break }
        if ($status -eq 'Running' -or $status -eq 'Paused') {
            try { $service.Stop() } catch { Write-Verbose -Message $_.Exception.Message }
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Warning -Message 'The btop-agent service did not stop within 20 seconds; ending its process.'
    Get-Process -Name $ProcessName -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    if (-not (Wait-AgentService -Status 'Stopped' -Seconds 10)) {
        throw 'The btop-agent service will not stop. Restart Windows, then run this command again.'
    }
}

# Gives any btop-agent.exe still running (the service process exiting, or a --probe someone
# started) up to 10 s to finish, then ends it so its files can be deleted.
function Stop-AgentProcess {
    $deadline = (Get-Date).AddSeconds(10)
    while (Get-Process -Name $ProcessName -ErrorAction SilentlyContinue) {
        if ((Get-Date) -ge $deadline) {
            Write-Warning -Message 'Ending btop-agent processes that are still running.'
            Get-Process -Name $ProcessName -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
            Start-Sleep -Seconds 1
            return
        }
        Start-Sleep -Milliseconds 500
    }
}

# Deletes a directory tree, retrying briefly while files are still in use. .NET's recursive
# delete removes junctions and symbolic links inside the tree without following them, which
# Remove-Item -Recurse in Windows PowerShell 5.1 does not guarantee.
function Remove-Tree {
    param([string]$Path)
    for ($attempt = 1; $attempt -le 5; $attempt++) {
        if (-not [IO.Directory]::Exists($Path)) { return }
        try {
            [IO.Directory]::Delete($Path, $true)
            return
        } catch {
            if ($attempt -eq 5) {
                throw ('Could not remove {0}: {1}' -f $Path, $_.Exception.GetBaseException().Message)
            }
            Start-Sleep -Seconds 1
        }
    }
}

$principal = New-Object -TypeName Security.Principal.WindowsPrincipal -ArgumentList ([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'This needs an elevated Windows PowerShell: right-click Windows PowerShell, choose "Run as administrator", and paste the command again.'
}

# The 64-bit Program Files, even if this is the 32-bit Windows PowerShell.
$programFiles = $env:ProgramW6432
if (-not $programFiles) { $programFiles = $env:ProgramFiles }
$programData = $env:ProgramData
if (-not $programData) { $programData = 'C:\ProgramData' }
$InstallDir = Join-Path -Path $programFiles -ChildPath 'btop-agent'
$DataDir = Join-Path -Path $programData -ChildPath 'btop-agent'

$removed = @()

if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host 'Stopping and deleting the btop-agent service...'
    Stop-AgentService
    # 1060: does not exist; 1072: already marked for deletion.
    Invoke-Native -FilePath $ScExe -ArgumentList @('delete', $ServiceName) -OkExitCodes @(0, 1060, 1072)
    if (Wait-AgentService -Status 'Gone' -Seconds 20) {
        $removed += 'service btop-agent'
    } else {
        Write-Warning -Message ('The btop-agent service is stopped and marked for deletion, but another program still has it open. ' +
            'It disappears once the Services console (services.msc), Event Viewer, Task Manager or Process Explorer is closed, or at the next restart.')
        $removed += 'service btop-agent (marked for deletion)'
    }
}

Stop-AgentProcess

foreach ($dir in @($InstallDir, $DataDir)) {
    if ([IO.Directory]::Exists($dir)) {
        Remove-Tree -Path $dir
        $removed += $dir
    }
}

Write-Host ''
if ($removed.Count -eq 0) {
    Write-Host 'Nothing to remove: btop-agent is not installed on this machine.'
} else {
    Write-Host 'btop-agent has been uninstalled. Removed:' -ForegroundColor Green
    foreach ($item in $removed) {
        Write-Host ('  ' + $item)
    }
}
