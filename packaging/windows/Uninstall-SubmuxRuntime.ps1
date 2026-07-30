[CmdletBinding()]
param([switch]$Purge)

$ErrorActionPreference = 'Stop'

function Stop-RuntimeService([string]$Name) {
    $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if ($null -eq $service) {
        return
    }
    if ($service.Status -ne 'Stopped') {
        Stop-Service -Name $Name -Force
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
}

Stop-RuntimeService 'SubmuxRuntime'
Stop-RuntimeService 'SubmuxRuntimeNet'

$product = Get-ChildItem -Path @(
    'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall',
    'HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall'
) -ErrorAction SilentlyContinue |
    Get-ItemProperty |
    Where-Object { $_.DisplayName -eq 'Submux Runtime' } |
    Select-Object -First 1
if ($null -eq $product -or [string]::IsNullOrWhiteSpace($product.PSChildName)) {
    throw 'Submux Runtime MSI registration was not found'
}
$process = Start-Process -FilePath msiexec.exe `
    -ArgumentList @('/x', $product.PSChildName, '/passive', '/norestart') `
    -Verb RunAs -Wait -PassThru
if ($process.ExitCode -notin @(0, 3010)) {
    throw "MSI uninstall failed with exit code $($process.ExitCode)"
}

$residuals = [Collections.Generic.List[string]]::new()
foreach ($name in @('SubmuxRuntime', 'SubmuxRuntimeNet')) {
    if (Get-Service -Name $name -ErrorAction SilentlyContinue) {
        $residuals.Add("service: $name")
    }
}
Get-NetRoute -ErrorAction SilentlyContinue |
    Where-Object { $_.InterfaceAlias -like '*submux*' } |
    ForEach-Object { $residuals.Add("route: $($_.DestinationPrefix) on $($_.InterfaceAlias)") }
Get-NetAdapter -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -like '*submux*' -or $_.InterfaceDescription -like '*submux*' } |
    ForEach-Object { $residuals.Add("interface: $($_.Name)") }
Get-NetFirewallRule -ErrorAction SilentlyContinue |
    Where-Object { $_.DisplayName -like '*submux*' -or $_.Group -like '*submux*' } |
    ForEach-Object { $residuals.Add("firewall: $($_.DisplayName)") }
if ($residuals.Count -gt 0) {
    throw ("Submux Runtime uninstall residuals:`n  " + ($residuals -join "`n  "))
} else {
    Write-Output 'No Runtime-owned service, route, firewall rule, or network interface residual was detected.'
}

if ($Purge) {
    $state = Join-Path $env:ProgramData 'SubmuxRuntime'
    if (Test-Path -LiteralPath $state) {
        $item = Get-Item -LiteralPath $state -Force
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Refusing to purge a reparse-point state directory: $state"
        }
        Remove-Item -LiteralPath $state -Recurse -Force
    }
}
