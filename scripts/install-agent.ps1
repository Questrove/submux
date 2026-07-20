[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(ParameterSetName = 'Install')]
    [Parameter(ParameterSetName = 'Upgrade')]
    [string]$Version,

    [Parameter(ParameterSetName = 'Install')]
    [Parameter(ParameterSetName = 'Upgrade')]
    [ValidateSet('stable', 'alpha')]
    [string]$Channel = 'stable',

    [Parameter(ParameterSetName = 'Install')]
    [switch]$Service,

    [Parameter(Mandatory, ParameterSetName = 'Upgrade')]
    [switch]$Upgrade,

    [Parameter(Mandatory, ParameterSetName = 'Rollback')]
    [switch]$Rollback,

    [Parameter(Mandatory, ParameterSetName = 'Uninstall')]
    [switch]$Uninstall,

    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\Submux"
)

$ErrorActionPreference = 'Stop'
$repo = 'Questrove/submux'
$target = Join-Path $InstallDir 'submux-agent.exe'
$previous = Join-Path $InstallDir '.submux-agent.previous.exe'
$startupDir = [Environment]::GetFolderPath('Startup')
$startupFile = Join-Path $startupDir 'submux-agent.cmd'
$startupMarker = 'rem Managed by submux-agent installer; do not edit.'

function Assert-NoReparsePath([string]$Path) {
    $current = [IO.Path]::GetFullPath($Path)
    while (-not (Test-Path -LiteralPath $current)) {
        $parent = [IO.Directory]::GetParent($current)
        if (-not $parent) { throw "No existing ancestor is available for $Path" }
        $current = $parent.FullName
    }
    while ($current) {
        $item = Get-Item -LiteralPath $current -Force
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$Path must not contain a reparse-point ancestor." }
        $parent = [IO.Directory]::GetParent($current)
        if (-not $parent) { break }
        $current = $parent.FullName
    }
}

function Assert-ManagedStartup {
    if (Test-Path -LiteralPath $startupFile) {
        $item = Get-Item -LiteralPath $startupFile -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Refusing to manage an invalid Agent startup entry.'
        }
        $firstLine = Get-Content -LiteralPath $startupFile -TotalCount 1
        if ($firstLine -ne $startupMarker) { throw 'Refusing to replace an unmanaged submux-agent.cmd startup entry.' }
    }
}

Assert-NoReparsePath $InstallDir
foreach ($managedPath in @($target, $previous)) {
    if (Test-Path -LiteralPath $managedPath) {
        $managedItem = Get-Item -LiteralPath $managedPath -Force
        if ($managedItem.PSIsContainer -or ($managedItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Refusing to use invalid managed path $managedPath"
        }
    }
}
Assert-ManagedStartup

if ($Uninstall) {
    Remove-Item -LiteralPath $startupFile -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $target, $previous -Force -ErrorAction SilentlyContinue
    Write-Host 'submux-agent was uninstalled; current-user state under LocalAppData\submux-agent was preserved.'
    return
}

if ($Rollback) {
    if (-not (Test-Path -LiteralPath $previous -PathType Leaf)) { throw 'No previous Agent binary is available.' }
    $failed = Join-Path $InstallDir ".submux-agent.failed.$PID.exe"
    Move-Item -LiteralPath $target -Destination $failed -Force
    try {
        Move-Item -LiteralPath $previous -Destination $target -Force
        Remove-Item -LiteralPath $failed -Force
    } catch {
        Move-Item -LiteralPath $failed -Destination $target -Force -ErrorAction SilentlyContinue
        throw
    }
    & $target --version
    return
}

if ($Upgrade -and -not (Test-Path -LiteralPath $target -PathType Leaf)) {
    throw "-Upgrade requires an existing $target"
}

if (-not $Version) {
    if ($Channel -eq 'stable') {
        $location = $null
        try {
            $latest = Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest" -MaximumRedirection 0
            $location = $latest.Headers.Location
        } catch {
            if ($_.Exception.Response) { $location = $_.Exception.Response.Headers['Location'] }
        }
        if (-not $location) { throw 'Could not resolve the latest stable release.' }
        $Version = Split-Path $location -Leaf
    } else {
        $releases = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases?per_page=50"
        $release = $releases | Where-Object { $_.prerelease -and -not $_.draft } | Select-Object -First 1
        if (-not $release) { throw 'No alpha release is available.' }
        $Version = $release.tag_name
    }
}
if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$') { throw "Invalid exact release version: $Version" }
if ($Channel -eq 'stable' -and $Version -match '(?i)(alpha|beta|rc|pre)') { throw "Pre-release $Version requires -Channel alpha" }

$arch = switch ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { throw "Unsupported Windows architecture: $_" }
}
$asset = "submux-agent-windows-$arch.exe"
$baseUrl = "https://github.com/$repo/releases/download/$Version"
$tempDir = Join-Path ([IO.Path]::GetTempPath()) "submux-agent-$([Guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Path $tempDir | Out-Null
try {
    $download = Join-Path $tempDir $asset
    $manifest = Join-Path $tempDir 'checksums.txt'
    Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $download
    Invoke-WebRequest -Uri "$baseUrl/checksums.txt" -OutFile $manifest
    $matchingLines = @(Get-Content -LiteralPath $manifest | Where-Object { $_ -match "\s\*?$([Regex]::Escape($asset))$" })
    if ($matchingLines.Count -ne 1) { throw "checksums.txt must contain exactly one entry for $asset" }
    $expected = (($matchingLines[0] -split '\s+')[0]).ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$') { throw "checksums.txt has an invalid digest for $asset" }
    $actual = (Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $asset" }
    $reported = & $download --version
    if ($LASTEXITCODE -ne 0 -or "$reported" -notmatch [Regex]::Escape(" $Version (")) { throw 'Downloaded binary version does not match the requested release.' }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Assert-NoReparsePath $InstallDir
    $staging = Join-Path $InstallDir ".submux-agent.staging.$PID.exe"
    Copy-Item -LiteralPath $download -Destination $staging -Force
    $hadPrevious = Test-Path -LiteralPath $target -PathType Leaf
    if ($hadPrevious) {
        Remove-Item -LiteralPath $previous -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $target -Destination $previous -Force
    }
    try {
        Move-Item -LiteralPath $staging -Destination $target -Force
        if ($Service) {
            New-Item -ItemType Directory -Path $startupDir -Force | Out-Null
            @($startupMarker, '@start "" /b "' + $target + '" serve') | Set-Content -LiteralPath $startupFile -Encoding Ascii
        }
    } catch {
        Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
        if ($hadPrevious) { Move-Item -LiteralPath $previous -Destination $target -Force }
        throw
    }
    & $target --version
    if ($Service) { Write-Host 'Current-user startup entry installed. Enroll this user, then start submux-agent serve once or sign in again.' }
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
