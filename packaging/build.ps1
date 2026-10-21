param([string]$OutputDirectory = (Join-Path $PSScriptRoot '..\..\packaging-output'), [switch]$SkipClientBuild)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$outputRoot = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force $outputRoot | Out-Null
& (Join-Path $PSScriptRoot 'generate-schema.ps1')

# 既存の依存関係だけを利用する。インストールやロックファイルの更新は行わない。
$env:GOPROXY = 'off'
$env:GOTOOLCHAIN = 'local'
if (-not $SkipClientBuild) { Push-Location (Join-Path $repoRoot 'client')
try {
    foreach ($variant in @('iptv', 'noiptv')) {
        $env:KONOMITV_IPTV = $(if ($variant -eq 'iptv') { '1' } else { '0' })
        $env:npm_package_version = 'akki-0.14.1-β0.1'
        & yarn --ignore-engines build --outDir (Join-Path $outputRoot ('client-' + $variant))
        if ($LASTEXITCODE -ne 0) { throw "Client build failed: $variant" }
    }
} finally { Pop-Location } }

$licenses = Join-Path $outputRoot 'licenses'
New-Item -ItemType Directory -Force $licenses | Out-Null
Copy-Item -LiteralPath (Join-Path $repoRoot 'License.txt') -Destination (Join-Path $licenses 'KonomiTV-MIT.txt')
$goLicenseDir = Join-Path $licenses 'go-runtime'
New-Item -ItemType Directory -Force $goLicenseDir | Out-Null
$goRoot = (& go env GOROOT).Trim()
Copy-Item -LiteralPath (Join-Path $goRoot 'LICENSE') -Destination $goLicenseDir
Copy-Item -LiteralPath (Join-Path $repoRoot 'server-go\internal\tsinfo\arib\THIRD_PARTY_NOTICES.md') -Destination (Join-Path $licenses 'ARIB-THIRD-PARTY-NOTICES.md')
Copy-Item -LiteralPath (Join-Path $repoRoot 'server-go\internal\tsinfo\arib\licenses\CPython.txt') -Destination (Join-Path $licenses 'CPython.txt')

Push-Location (Join-Path $repoRoot 'server-go')
try {
    $modules = & go list -m -f '{{.Path}}|{{.Version}}|{{.Dir}}' all
    if ($LASTEXITCODE -ne 0) { throw 'Cannot enumerate Go dependency licenses' }
    # 個人のモジュールキャッシュ絶対パスは配布しない。
    $modules | ForEach-Object { $fields = $_.Split('|'); $fields[0] + ' ' + $fields[1] } | Set-Content -LiteralPath (Join-Path $licenses 'go-modules.txt') -Encoding utf8
    foreach ($module in $modules) {
        $fields = $module.Split('|')
        if ($fields.Count -lt 3 -or -not $fields[2]) { continue }
        $moduleOut = Join-Path $licenses ('go/' + $fields[0] + '@' + $fields[1])
        New-Item -ItemType Directory -Force $moduleOut | Out-Null
        Get-ChildItem -LiteralPath $fields[2] -File | Where-Object Name -Match '^(LICENSE|COPYING|NOTICE|AUTHORS|PATENTS)' | ForEach-Object {
            Copy-Item -LiteralPath $_.FullName -Destination $moduleOut
        }
    }
} finally { Pop-Location }

# インストール済みclient依存のライセンス原文を保持する。個人設定等の任意ファイルはコピーしない。
$nodeRoot = Join-Path $repoRoot 'client\node_modules'
$nodeLicenseRoot = Join-Path $licenses 'client'
Get-ChildItem -LiteralPath $nodeRoot -Recurse -File -ErrorAction Stop | Where-Object Name -Match '^(LICENSE|LICENCE|COPYING|NOTICE)(\.|$|-)' | ForEach-Object {
    $relative = [IO.Path]::GetRelativePath($nodeRoot, $_.FullName)
    $destination = Join-Path $nodeLicenseRoot $relative
    New-Item -ItemType Directory -Force (Split-Path -Parent $destination) | Out-Null
    Copy-Item -LiteralPath $_.FullName -Destination $destination
}

Push-Location (Join-Path $repoRoot 'server-go')
try {
    foreach ($targetOS in @('windows', 'linux')) {
        & go run ./cmd/package-installer --os $targetOS --out $outputRoot
        if ($LASTEXITCODE -ne 0) { throw "Installer build failed: $targetOS" }
    }
} finally { Pop-Location }

Get-ChildItem -LiteralPath $outputRoot -File | Where-Object Name -Match '^KonomiTV-Go-Installer-' | ForEach-Object {
    '{0}  {1}' -f (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant(), $_.Name
} | Set-Content -LiteralPath (Join-Path $outputRoot 'SHA256SUMS') -Encoding ascii
