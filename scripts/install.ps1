# btop hub agent: install or upgrade on Windows.
#
# Paste into an elevated Windows PowerShell (Start, type "powershell", choose "Run as
# administrator"):
#
#   & ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('__HUB_URL__/install.ps1'))) -Token 'TOKEN' -Name 'Gaming rig'
#
#   -Token  the hub's agent token (required)
#   -Name   name shown on the hub, at most 48 bytes (required)
#   -Id     machine id: a-z, 0-9 and -, at most 32 characters (default: made from -Name)
#   -Hub    hub URL (default: the hub this script came from)
#
# It downloads btop-agent.exe from the hub and checks its SHA-256, installs it in
# "%ProgramFiles%\btop-agent", writes "%ProgramData%\btop-agent\agent.json" (it holds the token,
# so only SYSTEM, Administrators and LocalService can read it), and registers the "btop-agent"
# service: runs as NT AUTHORITY\LocalService, starts automatically (delayed) at boot, restarts
# after failures, logs to "%ProgramData%\btop-agent\agent.log". Then it runs the agent once with
# --probe and prints what it measured. Running it again upgrades in place (binary, token, name,
# id).
#
# Uninstall, also from an elevated Windows PowerShell:
#
#   & ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('__HUB_URL__/uninstall.ps1')))
#
# Keep this file plain ASCII and compatible with Windows PowerShell 5.1. It runs as a script
# block, where 'exit' would close the user's window, so failures throw instead.

param(
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$Name,
    [string]$Id = '',
    [string]$Hub = '__HUB_URL__'
)

$ErrorActionPreference = 'Stop'

$ServiceName = 'btop-agent'
$ProcessName = 'btop-agent'
$AgentSha256 = '__AGENT_SHA256__'
$LocalService = 'NT AUTHORITY\LocalService'
$System32 = Join-Path -Path $env:SystemRoot -ChildPath 'System32'
$ScExe = Join-Path -Path $System32 -ChildPath 'sc.exe'
$IcaclsExe = Join-Path -Path $System32 -ChildPath 'icacls.exe'

function Write-Step {
    param([string]$Text)
    Write-Host ''
    Write-Host ('==> ' + $Text) -ForegroundColor Cyan
}

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

# Prints a console program's output, whatever its exit code.
function Show-NativeOutput {
    param([string]$FilePath, [string[]]$ArgumentList)
    $ErrorActionPreference = 'Continue'
    try {
        & $FilePath @ArgumentList 2>&1 | ForEach-Object { Write-Host ('    ' + [string]$_) }
    } catch {
        Write-Host ('    ' + $_.Exception.Message)
    }
}

# Waits up to $Seconds for the service to reach $Status: 'Running', 'Stopped', or 'Gone' (no
# longer registered). Returns $true if it did.
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

# Deletes the service and waits until the service manager has really dropped it. It stays
# "marked for deletion" while another program (services.msc, Event Viewer, Task Manager...) has
# it open, and in that state it blocks creating the new one.
function Remove-AgentService {
    # 1060: does not exist; 1072: already marked for deletion.
    Invoke-Native -FilePath $ScExe -ArgumentList @('delete', $ServiceName) -OkExitCodes @(0, 1060, 1072)
    if (-not (Wait-AgentService -Status 'Gone' -Seconds 20)) {
        throw ('The old btop-agent service is marked for deletion, but another program still has it open. ' +
            'Close the Services console (services.msc), Event Viewer, Task Manager and Process Explorer, ' +
            'then run this command again (or restart Windows first).')
    }
}

# Gives any btop-agent.exe still running (the old service process exiting, or a --probe someone
# started) up to 10 s to finish, then ends it so the program file can be replaced.
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

# Runs the agent once as the current user and prints its report. Returns its exit code.
function Invoke-Probe {
    $ErrorActionPreference = 'Continue'
    # The agent writes UTF-8; without this, Windows PowerShell decodes it with the OEM code page.
    $savedEncoding = $null
    try {
        $savedEncoding = [Console]::OutputEncoding
        [Console]::OutputEncoding = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
    } catch {
        $savedEncoding = $null
    }
    $global:LASTEXITCODE = -1
    try {
        & $AgentExe --probe --config $ConfigPath 2>&1 | ForEach-Object { Write-Host ([string]$_) }
    } catch {
        Write-Host ('Could not run the probe: ' + $_.Exception.Message)
    } finally {
        if ($savedEncoding) {
            try { [Console]::OutputEncoding = $savedEncoding } catch { Write-Verbose -Message $_.Exception.Message }
        }
    }
    return $LASTEXITCODE
}

# ---- Checks -----------------------------------------------------------------------------------

$principal = New-Object -TypeName Security.Principal.WindowsPrincipal -ArgumentList ([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'This needs an elevated Windows PowerShell: right-click Windows PowerShell, choose "Run as administrator", and paste the command again.'
}
if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'btop-agent needs 64-bit Windows; this is a 32-bit installation.'
}

$Token = $Token.Trim()
$Name = $Name.Trim()
$Id = $Id.Trim()
$Hub = $Hub.Trim().TrimEnd('/')

if ($Hub -notmatch '^https?://[^\s''"]+\z') {
    throw ('-Hub must be the hub''s address, like http://192.168.1.74:28677 (got "{0}"). Copy the install command from the hub, or pass -Hub.' -f $Hub)
}
if ($Token -eq '' -or $Token -match '\s') {
    throw '-Token is empty or contains spaces; copy it again from the hub.'
}
if ($Name -eq '') {
    throw '-Name is empty.'
}
$nameBytes = [Text.Encoding]::UTF8.GetByteCount($Name)
if ($nameBytes -gt 48) {
    throw ('-Name "{0}" is too long: the hub allows 48 bytes (48 plain letters), and it is {1}.' -f $Name, $nameBytes)
}
if ($Id -eq '') {
    # Same rule as the agent's default: lower case, every run of anything but a-z and 0-9
    # becomes '-', no '-' at either end, at most 32 characters.
    $Id = ($Name.ToLowerInvariant() -creplace '[^a-z0-9]+', '-').Trim('-')
    if ($Id.Length -gt 32) {
        $Id = $Id.Substring(0, 32).TrimEnd('-')
    }
}
if ($Id -cnotmatch '^[a-z0-9][a-z0-9-]{0,31}\z') {
    throw ('Machine id "{0}" is not usable. Pass -Id with 1 to 32 characters from a-z, 0-9 and -, starting with a letter or digit, for example -Id ''gaming-rig''.' -f $Id)
}
$expectedSha256 = $AgentSha256.Trim().ToLowerInvariant()
if ($expectedSha256 -cnotmatch '^[0-9a-f]{64}\z') {
    throw ('The hub gave no checksum for btop-agent.exe ("{0}"): its dist directory has no Windows agent. Put btop-agent.exe there, restart the hub, and copy the install command from the hub again.' -f $AgentSha256)
}

# The 64-bit Program Files, even if this is the 32-bit Windows PowerShell.
$programFiles = $env:ProgramW6432
if (-not $programFiles) { $programFiles = $env:ProgramFiles }
$programData = $env:ProgramData
if (-not $programData) { $programData = 'C:\ProgramData' }
$InstallDir = Join-Path -Path $programFiles -ChildPath 'btop-agent'
$AgentExe = Join-Path -Path $InstallDir -ChildPath 'btop-agent.exe'
$DownloadPath = $AgentExe + '.download'
$DataDir = Join-Path -Path $programData -ChildPath 'btop-agent'
$ConfigPath = Join-Path -Path $DataDir -ChildPath 'agent.json'
$LogPath = Join-Path -Path $DataDir -ChildPath 'agent.log'

# The hub speaks plain HTTP, but a proxy in between might want TLS 1.2. SystemDefault (0) already
# lets Windows choose TLS 1.2 or 1.3, so only an explicit, older list is extended.
try {
    $protocols = [Net.ServicePointManager]::SecurityProtocol
    if ([int]$protocols -ne 0) {
        [Net.ServicePointManager]::SecurityProtocol = $protocols -bor [Net.SecurityProtocolType]::Tls12
    }
} catch {
    Write-Verbose -Message ('Could not enable TLS 1.2: ' + $_.Exception.Message)
}

Write-Host ('Installing btop-agent for "{0}" (id {1}), reporting to {2}' -f $Name, $Id, $Hub)

# ---- Download and verify, before touching the running agent -----------------------------------

$agentUrl = $Hub + '/dist/btop-agent.exe'
Write-Step ('Downloading ' + $agentUrl)
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
$client = New-Object -TypeName System.Net.WebClient
try {
    # Next to the target, so it ends up with that directory's permissions and moves atomically.
    $client.DownloadFile($agentUrl, $DownloadPath)
} catch {
    Remove-Item -LiteralPath $DownloadPath -Force -ErrorAction SilentlyContinue
    throw ('Could not download {0}: {1}' -f $agentUrl, $_.Exception.GetBaseException().Message)
} finally {
    $client.Dispose()
}
$actualSha256 = (Get-FileHash -LiteralPath $DownloadPath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualSha256 -cne $expectedSha256) {
    Remove-Item -LiteralPath $DownloadPath -Force -ErrorAction SilentlyContinue
    throw ('btop-agent.exe from {0} has SHA-256 {1}, but the hub announced {2}. Nothing was changed. If the agent on the hub was just rebuilt, restart the hub and copy the install command again.' -f $Hub, $actualSha256, $expectedSha256)
}
Write-Host ('    SHA-256 verified: ' + $actualSha256)

# ---- Replace the service and the program -------------------------------------------------------

# The service is deleted and created again rather than edited: sc.exe config binPath= cannot be
# given a path with embedded quotes reliably from Windows PowerShell 5.1.
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Step 'Stopping and removing the existing btop-agent service (it is created again below)'
    Stop-AgentService
    Remove-AgentService
}
Stop-AgentProcess

Write-Step ('Installing ' + $AgentExe)
$installError = $null
for ($attempt = 1; $attempt -le 10; $attempt++) {
    try {
        [IO.File]::Delete($AgentExe)
        [IO.File]::Move($DownloadPath, $AgentExe)
        $installError = $null
        break
    } catch {
        $installError = $_.Exception.GetBaseException().Message
        Start-Sleep -Milliseconds 500
    }
}
if ($installError) {
    Remove-Item -LiteralPath $DownloadPath -Force -ErrorAction SilentlyContinue
    throw ('Could not replace {0}: {1}' -f $AgentExe, $installError)
}

# ---- Configuration -----------------------------------------------------------------------------

Write-Step ('Writing ' + $ConfigPath)
# Any user may create folders in ProgramData. If something other than a plain folder is already
# here (a junction someone planted), remove the link so the token cannot be steered elsewhere.
if ([IO.Directory]::Exists($DataDir) -and ([IO.File]::GetAttributes($DataDir)).HasFlag([IO.FileAttributes]::ReparsePoint)) {
    [IO.Directory]::Delete($DataDir)
}
New-Item -ItemType Directory -Path $DataDir -Force | Out-Null
# icacls takes SIDs (*S-...) because account names are localized: S-1-5-18 SYSTEM, S-1-5-32-544
# Administrators, S-1-5-19 LocalService. The folder: nothing inherited and no leftover entries,
# SYSTEM and Administrators full control, LocalService modify (the service writes its log here),
# owned by Administrators.
Invoke-Native -FilePath $IcaclsExe -ArgumentList @($DataDir, '/reset')
Invoke-Native -FilePath $IcaclsExe -ArgumentList @($DataDir, '/inheritance:r', '/grant:r', '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-19:(OI)(CI)M')
Invoke-Native -FilePath $IcaclsExe -ArgumentList @($DataDir, '/setowner', '*S-1-5-32-544')

$config = [ordered]@{
    hub   = $Hub
    token = $Token
    id    = $Id
    name  = $Name
}
$json = ConvertTo-Json -InputObject $config
# A fresh file starts with the folder's permissions; UTF-8 without a byte order mark.
[IO.File]::Delete($ConfigPath)
[IO.File]::WriteAllText($ConfigPath, $json, (New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false))
# The token file: SYSTEM and Administrators full control, LocalService read, nobody else.
Invoke-Native -FilePath $IcaclsExe -ArgumentList @($ConfigPath, '/inheritance:r', '/grant:r', '*S-1-5-18:F', '*S-1-5-32-544:F', '*S-1-5-19:R')

# ---- Service -----------------------------------------------------------------------------------

Write-Step 'Registering the btop-agent service'
# New-Service passes the quoted path through intact. The account is set here, with an empty
# password, because Windows PowerShell 5.1 drops an empty "" argument to sc.exe (password= "").
$noPassword = New-Object -TypeName System.Security.SecureString
$credential = New-Object -TypeName System.Management.Automation.PSCredential -ArgumentList $LocalService, $noPassword
$serviceArgs = @{
    Name           = $ServiceName
    DisplayName    = 'btop hub agent'
    Description    = 'Streams this machine''s CPU, GPU, memory, disk and network activity to the btop hub.'
    BinaryPathName = ('"{0}" --config "{1}"' -f $AgentExe, $ConfigPath)
    StartupType    = 'Automatic'
    Credential     = $credential
}
try {
    New-Service @serviceArgs | Out-Null
} catch {
    throw ('Could not create the btop-agent service: {0} If it says the service is marked for deletion, close services.msc, Event Viewer and Task Manager (or restart Windows), then run this command again.' -f $_.Exception.Message)
}
$serviceKey = 'HKLM:\SYSTEM\CurrentControlSet\Services\' + $ServiceName
$account = [string](Get-ItemProperty -LiteralPath $serviceKey -Name 'ObjectName').ObjectName
$isLocalService = (($account -replace '\s', '') -eq 'NTAUTHORITY\LocalService')
if (-not $isLocalService) {
    # In case the name was stored localized: compare the account's SID instead.
    try {
        $accountSid = (New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList $account).Translate([System.Security.Principal.SecurityIdentifier]).Value
        $isLocalService = ($accountSid -eq 'S-1-5-19')
    } catch {
        $isLocalService = $false
    }
}
if (-not $isLocalService) {
    throw ('The btop-agent service was registered to run as "{0}" instead of {1}; not starting it.' -f $account, $LocalService)
}
Invoke-Native -FilePath $ScExe -ArgumentList @('config', $ServiceName, 'start=', 'delayed-auto')
Invoke-Native -FilePath $ScExe -ArgumentList @('failure', $ServiceName, 'reset=', '86400', 'actions=', 'restart/5000/restart/5000/restart/60000')
# Also restart when the agent stops itself with an error exit code, not only when it crashes.
Invoke-Native -FilePath $ScExe -ArgumentList @('failureflag', $ServiceName, '1')

Write-Step 'Starting the service'
$startError = $null
try {
    (Get-Service -Name $ServiceName).Start()
} catch {
    $startError = $_.Exception.GetBaseException().Message
}
$running = $false
if (-not $startError -and (Wait-AgentService -Status 'Running' -Seconds 15)) {
    # An agent that fails right after starting is Running for a moment; look again.
    Start-Sleep -Seconds 3
    $running = Wait-AgentService -Status 'Running' -Seconds 0
}
if (-not $running) {
    Write-Host ''
    Write-Host 'The btop-agent service is not running.' -ForegroundColor Red
    if ($startError) {
        Write-Host ('Starting it failed: ' + $startError)
    }
    Write-Host 'Service status (sc.exe queryex btop-agent):'
    Show-NativeOutput -FilePath $ScExe -ArgumentList @('queryex', $ServiceName)
    Write-Host ('Last 30 lines of ' + $LogPath + ':')
    try {
        if (Test-Path -LiteralPath $LogPath) {
            Get-Content -LiteralPath $LogPath -Tail 30 -Encoding UTF8 | ForEach-Object { Write-Host ('    ' + $_) }
        } else {
            Write-Host '    (no log file yet)'
        }
    } catch {
        Write-Host ('    (could not read it: ' + $_.Exception.Message + ')')
    }
    throw 'btop-agent is installed, but its service is not running; see the status and log above.'
}
Write-Host '    Running.'

# ---- Probe and summary -------------------------------------------------------------------------

Write-Step 'Probing this machine once, as you (the service keeps running in the background)'
$probeExit = Invoke-Probe
if ($probeExit -ne 0) {
    Write-Warning -Message ('The probe exited with code {0}. The service is running, but look at the output above.' -f $probeExit)
}

$serviceStatus = [string](Get-Service -Name $ServiceName).Status
Write-Host ''
Write-Host 'btop-agent is installed.' -ForegroundColor Green
Write-Host ('  Service : {0} is {1}; starts automatically (delayed) at boot as {2}' -f $ServiceName, $serviceStatus, $LocalService)
Write-Host ('  Machine : {0} (id {1})' -f $Name, $Id)
Write-Host ('  Hub     : {0}' -f $Hub)
Write-Host ('  Program : {0}' -f $AgentExe)
Write-Host ('  Config  : {0}' -f $ConfigPath)
Write-Host ('  Log     : {0}' -f $LogPath)
Write-Host ''
Write-Host 'To uninstall, run this in an elevated Windows PowerShell:'
Write-Host ('  & ([scriptblock]::Create((New-Object Net.WebClient).DownloadString(''{0}/uninstall.ps1'')))' -f $Hub)
Write-Host ''
Write-Host 'If anything in the probe output above looks wrong, paste it back to Claude.'
