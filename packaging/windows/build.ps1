[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^v\d+\.\d+\.\d+$')][string]$Version,
    [Parameter(Mandatory = $true)][ValidateSet('amd64', 'arm64')][string]$Architecture,
    [Parameter(Mandatory = $true)][ValidateSet('online', 'offline')][string]$Kind,
    [Parameter(Mandatory = $true)][string]$Runtime,
    [Parameter(Mandatory = $true)][string]$RuntimeNet,
    [Parameter(Mandatory = $true)][string]$Gui,
    [Parameter(Mandatory = $true)][string]$Sbom,
    [Parameter(Mandatory = $true)][string]$License,
    [Parameter(Mandatory = $true)][string]$OutputDirectory,
    [string]$Mihomo,
    [string]$TufDirectory,
    [string]$Wix = 'wix'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Resolve-RegularFile([string]$Name, [string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw "$Name is required"
    }
    $item = Get-Item -LiteralPath $Path -Force
    if (-not $item.PSIsContainer -and -not ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        return $item.FullName
    }
    throw "$Name must be a regular non-linked file"
}

function Resolve-RealDirectory([string]$Name, [string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw "$Name is required"
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer -and -not ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        return $item.FullName
    }
    throw "$Name must be a real non-linked directory"
}

function Assert-PeArchitecture([string]$Name, [string]$Path, [string]$ExpectedArchitecture) {
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        if ($stream.Length -lt 256) {
            throw "$Name is not a valid PE executable"
        }
        $reader = [IO.BinaryReader]::new($stream)
        if ($reader.ReadUInt16() -ne 0x5A4D) {
            throw "$Name is not a valid PE executable"
        }
        $stream.Position = 0x3C
        $header = $reader.ReadInt32()
        if ($header -lt 0 -or $header -gt ($stream.Length - 6)) {
            throw "$Name has an invalid PE header"
        }
        $stream.Position = $header
        if ($reader.ReadUInt32() -ne 0x00004550) {
            throw "$Name is not a valid PE executable"
        }
        $machine = $reader.ReadUInt16()
        $expected = if ($ExpectedArchitecture -eq 'amd64') { 0x8664 } else { 0xAA64 }
        if ($machine -ne $expected) {
            throw "$Name does not match the requested $ExpectedArchitecture architecture"
        }
    } finally {
        $stream.Dispose()
    }
}

$runtimePath = Resolve-RegularFile 'Runtime' $Runtime
$networkPath = Resolve-RegularFile 'RuntimeNet' $RuntimeNet
$guiPath = Resolve-RegularFile 'Gui' $Gui
$sbomPath = Resolve-RegularFile 'Sbom' $Sbom
$licensePath = Resolve-RegularFile 'License' $License
Assert-PeArchitecture 'Runtime' $runtimePath $Architecture
Assert-PeArchitecture 'RuntimeNet' $networkPath $Architecture
Assert-PeArchitecture 'Gui' $guiPath $Architecture
if ((Get-Item -LiteralPath $sbomPath).Length -eq 0 -or
    (Get-Item -LiteralPath $licensePath).Length -eq 0) {
    throw 'SBOM and license inputs must not be empty'
}
[void](Get-Content -Raw -LiteralPath $sbomPath | ConvertFrom-Json)

if ($Kind -eq 'offline') {
    $mihomoPath = Resolve-RegularFile 'Mihomo' $Mihomo
    Assert-PeArchitecture 'Mihomo' $mihomoPath $Architecture
    $tufPath = Resolve-RealDirectory 'TufDirectory' $TufDirectory
    $tufFiles = @{}
    foreach ($name in @('root.json', 'timestamp.json', 'snapshot.json', 'targets.json')) {
        $tufFiles[$name] = Resolve-RegularFile "TUF $name" (Join-Path $tufPath $name)
        $metadata = Get-Content -Raw -LiteralPath $tufFiles[$name] | ConvertFrom-Json
        if ($null -eq $metadata.signed -or $null -eq $metadata.signatures) {
            throw "TUF $name must contain signed metadata and signatures"
        }
    }
} elseif (-not [string]::IsNullOrWhiteSpace($Mihomo) -or -not [string]::IsNullOrWhiteSpace($TufDirectory)) {
    throw 'Online MSI packages must not contain Mihomo or an offline TUF bundle'
}

$outputPath = [IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($outputPath) | Out-Null
$work = Join-Path ([IO.Path]::GetTempPath()) ('submux-msi-' + [guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($work) | Out-Null
try {
    $productVersion = $Version.Substring(1)
    $manifestInputs = [ordered]@{
        'submux-runtime.exe' = $runtimePath
        'submux-runtime-net.exe' = $networkPath
        'submux-runtime-gui.exe' = $guiPath
        'SBOM.spdx.json' = $sbomPath
        'licenses/NOTICE.txt' = $licensePath
    }
    if ($Kind -eq 'offline') {
        $manifestInputs['offline/mihomo.exe'] = $mihomoPath
        foreach ($name in $tufFiles.Keys) {
            $manifestInputs["offline/tuf/$name"] = $tufFiles[$name]
        }
    }
    $manifestPath = Join-Path $work 'ARTIFACT-MANIFEST.sha256'
    $manifestLines = foreach ($entry in $manifestInputs.GetEnumerator()) {
        $hash = (Get-FileHash -LiteralPath $entry.Value -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $($entry.Key)"
    }
    [IO.File]::WriteAllLines($manifestPath, $manifestLines, [Text.UTF8Encoding]::new($false))

    $source = Join-Path $PSScriptRoot 'Product.wxs'
    $artifact = Join-Path $outputPath "submux-runtime_${productVersion}_${Architecture}_${Kind}.msi"
    $wixArch = if ($Architecture -eq 'amd64') { 'x64' } else { 'arm64' }
    $definitions = @(
        "-d", "ProductVersion=$productVersion",
        "-d", "PackageKind=$Kind",
        "-d", "RuntimePath=$runtimePath",
        "-d", "NetworkPath=$networkPath",
        "-d", "GuiPath=$guiPath",
        "-d", "SbomPath=$sbomPath",
        "-d", "LicensePath=$licensePath",
        "-d", "ManifestPath=$manifestPath"
    )
    if ($Kind -eq 'offline') {
        $definitions += @(
            '-d', "MihomoPath=$mihomoPath",
            '-d', "TufRootPath=$($tufFiles['root.json'])",
            '-d', "TufTimestampPath=$($tufFiles['timestamp.json'])",
            '-d', "TufSnapshotPath=$($tufFiles['snapshot.json'])",
            '-d', "TufTargetsPath=$($tufFiles['targets.json'])"
        )
    }
    & $Wix build -arch $wixArch -ext WixToolset.Util.wixext @definitions -o $artifact $source
    if ($LASTEXITCODE -ne 0) {
        throw "WiX failed with exit code $LASTEXITCODE"
    }
    $artifactHash = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$artifact.sha256", "$artifactHash  $([IO.Path]::GetFileName($artifact))`n", [Text.UTF8Encoding]::new($false))
    Write-Output $artifact
} finally {
    if (Test-Path -LiteralPath $work) {
        Remove-Item -LiteralPath $work -Recurse -Force
    }
}
