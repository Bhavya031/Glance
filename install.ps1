param(
    [string]$Version,
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\Glance\bin')
)

$ErrorActionPreference = 'Stop'
$repo = 'Bhavya031/Glance'
if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
}
if (-not [Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::Windows)) {
    throw 'This installer only supports Windows.'
}
$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
$arch = switch ($architecture) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { throw "Unsupported Windows architecture: $architecture" }
}

if (-not $Version) {
    $Version = (Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest").tag_name
}
if ($Version -notmatch '^v[0-9A-Za-z][0-9A-Za-z._+-]*$') {
    throw "Invalid release version: $Version"
}

$release = $Version.Substring(1)
$asset = "Glance_${release}_windows_${arch}.zip"
$base = "https://github.com/$repo/releases/download/$Version"
$temp = Join-Path ([IO.Path]::GetTempPath()) ("glance-" + [guid]::NewGuid())

try {
    New-Item -ItemType Directory -Path $temp | Out-Null
    $archive = Join-Path $temp $asset
    $checksums = Join-Path $temp 'checksums.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $checksums
    $checksumMatches = @(Get-Content $checksums | ForEach-Object {
        if ($_ -match ('^([0-9A-Fa-f]{64})\s+\*?' + [regex]::Escape($asset) + '$')) { $Matches[1] }
    })
    if ($checksumMatches.Count -ne 1) { throw "Missing or invalid checksum for $asset." }
    if ((Get-FileHash -Algorithm SHA256 $archive).Hash -ne $checksumMatches[0]) {
        throw "Checksum verification failed for $asset."
    }

    Expand-Archive -LiteralPath $archive -DestinationPath $temp
    $binary = Join-Path $temp 'glance.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'glance.exe is missing from the release archive.' }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -LiteralPath $binary -Destination (Join-Path $InstallDir 'glance.exe') -Force
    Write-Host "Installed glance at $(Join-Path $InstallDir 'glance.exe')"
    if (($env:Path -split ';') -contains $InstallDir) {
        Write-Host 'glance is ready to use.'
    } else {
        Write-Host "Add $InstallDir to your user PATH, then run glance."
    }
} finally {
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Recurse -Force }
}
