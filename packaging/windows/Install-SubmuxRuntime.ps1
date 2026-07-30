[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Msi,
    [switch]$AuthorizeCurrentUser,
    [switch]$AllowDowngrade,
    [switch]$DatabaseCompatible
)

$ErrorActionPreference = 'Stop'
$msiPath = (Get-Item -LiteralPath $Msi -Force).FullName
$arguments = @('/i', "`"$msiPath`"")
if ($AuthorizeCurrentUser) {
    $arguments += 'AUTHORIZE_CURRENT_USER=1'
}
if ($AllowDowngrade -or $DatabaseCompatible) {
    if (-not ($AllowDowngrade -and $DatabaseCompatible)) {
        throw 'Downgrade requires both -AllowDowngrade and -DatabaseCompatible'
    }
    $arguments += @('ALLOW_DOWNGRADE=1', 'DATABASE_COMPATIBLE=1')
}
$process = Start-Process -FilePath msiexec.exe -ArgumentList $arguments -Verb RunAs -Wait -PassThru
exit $process.ExitCode
